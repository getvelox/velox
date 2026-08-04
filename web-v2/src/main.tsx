import { StrictMode, lazy, Suspense, Component } from 'react'
import type { ReactNode, ErrorInfo } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom'
import { Toaster } from 'sonner'
import { TooltipProvider } from '@/components/ui/tooltip'
import { AuthProvider, useAuth } from '@/contexts/AuthContext'
import { ModeAwareQueryProvider } from '@/contexts/QueryProvider'
import { TenantTimezoneBootstrap } from '@/lib/TenantTimezoneBootstrap'
import '@fontsource-variable/geist/index.css'
import '@fontsource-variable/geist-mono/index.css'
import './index.css'

// Error Boundary
class ErrorBoundary extends Component<
  { children: ReactNode },
  { hasError: boolean; error?: Error }
> {
  state = { hasError: false, error: undefined as Error | undefined }

  static getDerivedStateFromError(error: Error) {
    return { hasError: true, error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('ErrorBoundary:', error, info)
  }

  render() {
    if (this.state.hasError) {
      return (
        <div className="min-h-screen flex items-center justify-center p-8">
          <div className="text-center max-w-md">
            <h1 className="text-lg font-semibold mb-2 text-foreground">Something went wrong</h1>
            <p className="text-sm text-muted-foreground mb-4">{this.state.error?.message}</p>
            <button
              onClick={() => {
                this.setState({ hasError: false })
                window.location.href = '/'
              }}
              className="px-4 py-2 bg-primary text-primary-foreground rounded-lg text-sm"
            >
              Return to Dashboard
            </button>
          </div>
        </div>
      )
    }
    return this.props.children
  }
}

function ProtectedRoute({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth()
  if (loading) {
    return (
      <div className="min-h-screen flex items-center justify-center">
        <div className="animate-spin h-8 w-8 border-2 border-primary border-t-transparent rounded-full" />
      </div>
    )
  }
  if (!user) {
    return <Navigate to="/login" replace />
  }
  return <>{children}</>
}

// Mirror of ProtectedRoute for pages that only make sense when logged out
// (login, forgot-password). A signed-in user landing on /login is sent to the
// dashboard instead of being shown a sign-in form for an account they're
// already in. Token-driven flows (reset-password, accept-invite) deliberately
// skip this — those tokens are the proof of intent and may apply to a
// different account than the active session.
function PublicOnlyRoute({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth()
  if (loading) {
    return (
      <div className="min-h-screen flex items-center justify-center">
        <div className="animate-spin h-8 w-8 border-2 border-primary border-t-transparent rounded-full" />
      </div>
    )
  }
  if (user) {
    return <Navigate to="/" replace />
  }
  return <>{children}</>
}

// Lazy load pages
const LoginPage = lazy(() => import('@/pages/Login'))
const ForgotPasswordPage = lazy(() => import('@/pages/ForgotPassword'))
const ResetPasswordPage = lazy(() => import('@/pages/ResetPassword'))
const AcceptInvitePage = lazy(() => import('@/pages/AcceptInvite'))
const DashboardPage = lazy(() => import('@/pages/Dashboard'))
const CustomersPage = lazy(() => import('@/pages/Customers'))
const PricingPage = lazy(() => import('@/pages/Pricing'))
const DunningPage = lazy(() => import('@/pages/Dunning'))
const DunningPoliciesPage = lazy(() => import('@/pages/DunningPolicies'))
const WebhooksPage = lazy(() => import('@/pages/Webhooks'))
const WebhookEventsPage = lazy(() => import('@/pages/WebhookEvents'))
const ApiKeysPage = lazy(() => import('@/pages/ApiKeys'))
const TestClocksPage = lazy(() => import('@/pages/TestClocks'))
const TestClockDetailPage = lazy(() => import('@/pages/TestClockDetail'))
const SettingsPage = lazy(() => import('@/pages/Settings'))
const InvoicesPage = lazy(() => import('@/pages/Invoices'))
const SubscriptionsPage = lazy(() => import('@/pages/Subscriptions'))
const UsageEventsPage = lazy(() => import('@/pages/UsageEvents'))
const CreditsPage = lazy(() => import('@/pages/Credits'))
const ProviderCostsPage = lazy(() => import('@/pages/ProviderCosts'))
const CreditNotesPage = lazy(() => import('@/pages/CreditNotes'))
const AuditLogPage = lazy(() => import('@/pages/AuditLog'))
const UpdatePaymentPage = lazy(() => import('@/pages/UpdatePayment'))
const PaymentMethodAddedPage = lazy(() => import('@/pages/PaymentMethodAdded'))
const HostedInvoicePage = lazy(() => import('@/pages/HostedInvoice'))
const CustomerDetailPage = lazy(() => import('@/pages/CustomerDetail'))
const InvoiceDetailPage = lazy(() => import('@/pages/InvoiceDetail'))
const SubscriptionDetailPage = lazy(() => import('@/pages/SubscriptionDetail'))
const PlanDetailPage = lazy(() => import('@/pages/PlanDetail'))
const MeterDetailPage = lazy(() => import('@/pages/MeterDetail'))
const OnboardingPage = lazy(() => import('@/pages/Onboarding'))
const RecipesPage = lazy(() => import('@/pages/Recipes'))

const App = () => (
  <ErrorBoundary>
    <AuthProvider>
      <ModeAwareQueryProvider>
      <TenantTimezoneBootstrap />
      <TooltipProvider>
        <BrowserRouter>
          <Suspense
            fallback={
              <div className="min-h-screen flex items-center justify-center">
                <div className="animate-spin h-8 w-8 border-2 border-primary border-t-transparent rounded-full" />
              </div>
            }
          >
            <Routes>
              <Route path="/login" element={<PublicOnlyRoute><LoginPage /></PublicOnlyRoute>} />
              <Route path="/forgot-password" element={<PublicOnlyRoute><ForgotPasswordPage /></PublicOnlyRoute>} />
              <Route path="/reset-password" element={<PublicOnlyRoute><ResetPasswordPage /></PublicOnlyRoute>} />
              {/* Token-driven: no PublicOnlyRoute — see comment above. */}
              <Route path="/accept-invite" element={<AcceptInvitePage />} />
              <Route path="/" element={<ProtectedRoute><DashboardPage /></ProtectedRoute>} />
              <Route path="/onboarding" element={<ProtectedRoute><OnboardingPage /></ProtectedRoute>} />
              <Route path="/recipes" element={<ProtectedRoute><RecipesPage /></ProtectedRoute>} />
              <Route path="/customers" element={<ProtectedRoute><CustomersPage /></ProtectedRoute>} />
              <Route path="/pricing" element={<ProtectedRoute><PricingPage /></ProtectedRoute>} />
              <Route path="/dunning" element={<ProtectedRoute><DunningPage /></ProtectedRoute>} />
              <Route path="/dunning-policies" element={<ProtectedRoute><DunningPoliciesPage /></ProtectedRoute>} />
              <Route path="/webhooks" element={<ProtectedRoute><WebhooksPage /></ProtectedRoute>} />
              <Route path="/webhooks/events" element={<ProtectedRoute><WebhookEventsPage /></ProtectedRoute>} />
              <Route path="/api-keys" element={<ProtectedRoute><ApiKeysPage /></ProtectedRoute>} />
              <Route path="/test-clocks" element={<ProtectedRoute><TestClocksPage /></ProtectedRoute>} />
              <Route path="/test-clocks/:id" element={<ProtectedRoute><TestClockDetailPage /></ProtectedRoute>} />
              <Route path="/invoices" element={<ProtectedRoute><InvoicesPage /></ProtectedRoute>} />
              <Route path="/subscriptions" element={<ProtectedRoute><SubscriptionsPage /></ProtectedRoute>} />
              <Route path="/usage" element={<ProtectedRoute><UsageEventsPage /></ProtectedRoute>} />
              <Route path="/credits" element={<ProtectedRoute><CreditsPage /></ProtectedRoute>} />
              <Route path="/provider-costs" element={<ProtectedRoute><ProviderCostsPage /></ProtectedRoute>} />
              <Route path="/credit-notes" element={<ProtectedRoute><CreditNotesPage /></ProtectedRoute>} />
              <Route path="/audit-log" element={<ProtectedRoute><AuditLogPage /></ProtectedRoute>} />
              <Route path="/settings" element={<ProtectedRoute><SettingsPage /></ProtectedRoute>} />
              <Route path="/customers/:id" element={<ProtectedRoute><CustomerDetailPage /></ProtectedRoute>} />
              <Route path="/invoices/:id" element={<ProtectedRoute><InvoiceDetailPage /></ProtectedRoute>} />
              <Route path="/subscriptions/:id" element={<ProtectedRoute><SubscriptionDetailPage /></ProtectedRoute>} />
              <Route path="/plans/:id" element={<ProtectedRoute><PlanDetailPage /></ProtectedRoute>} />
              <Route path="/meters/:id" element={<ProtectedRoute><MeterDetailPage /></ProtectedRoute>} />
              <Route path="/update-payment" element={<UpdatePaymentPage />} />
              {/* Stripe Checkout setup-session return URL.
                  Public, unauthenticated — customers arrive here via
                  the email link → Stripe Checkout → here, with no
                  portal session. Renders success/cancel copy based
                  on ?status=success|cancel query. */}
              <Route path="/payment-method-added" element={<PaymentMethodAddedPage />} />
              <Route path="/invoice/:token" element={<HostedInvoicePage />} />
              {/* Customer-facing self-service portal — magic-link auth, no
                  operator dashboard chrome. /portal/login + /portal/magic
                  are unauthenticated; /portal reads the session token from
                  localStorage and redirects to /portal/login on miss. */}
            </Routes>
          </Suspense>
        </BrowserRouter>
      </TooltipProvider>
      </ModeAwareQueryProvider>
      {/* Outside ModeAwareQueryProvider on purpose. That provider is
          keyed on livemode to force a remount when the mode flips; a
          Toaster inside it is unmounted in the same commit that the
          mode-toggle handler fires its "Switched to live mode" toast,
          so the toast never renders. Sonner keeps its queue in a
          module-level store, so a remounted Toaster starts empty. */}
      <Toaster position="bottom-right" richColors closeButton />
    </AuthProvider>
  </ErrorBoundary>
)

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
