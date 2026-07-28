import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useQuery } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { formatCents } from '@/lib/api'

import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'

import { CreditCard, AlertTriangle, ExternalLink, ShieldCheck, Clock, Loader2, Info } from 'lucide-react'

interface TokenData {
  customer_name: string
  invoice_number: string
  // Terminal invoice states must not be dressed as "you owe this" —
  // the link outlives the invoice's collectible life (24h token; the
  // operator can void or settle at any moment).
  invoice_status?: string
  amount_due_cents: number
  currency: string
  branding?: {
    company_name?: string
    logo_url?: string
    brand_color?: string
    support_url?: string
  }
}

// Server-validated on save; re-checked before inline styles (same
// defence-in-depth as HostedInvoice).
function isHexColor(c: string | undefined): c is string {
  return !!c && /^#[0-9a-f]{6}$/i.test(c)
}

export default function UpdatePaymentPage() {
  usePageTitle('Update payment method')
  const [searchParams] = useSearchParams()
  const token = searchParams.get('token') || ''

  const [redirecting, setRedirecting] = useState(false)
  // submitError surfaces failures from the POST /checkout call inline
  // on the page (Stripe not connected, payment setup missing, etc.) —
  // distinct from `error` which is the "link is invalid" terminal
  // state. We keep the rest of the UI rendered so the customer can see
  // the invoice context that the error refers to and retry.
  const [submitError, setSubmitError] = useState('')

  // Token validation. Single-use endpoint, but RQ still gives us
  // proper loading/error state without per-mount thrash and matches
  // the rest of the app's data-fetching pattern. retry=false so a
  // genuinely invalid token doesn't pound the server.
  const tokenQuery = useQuery({
    queryKey: ['public-payment-update', token],
    queryFn: async (): Promise<TokenData> => {
      const apiBase = import.meta.env.VITE_API_URL || ''
      const res = await fetch(`${apiBase}/v1/public/payment-updates/${encodeURIComponent(token)}`)
      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        throw new Error(body?.error?.message || 'This link has expired or is invalid')
      }
      return res.json()
    },
    enabled: !!token,
    retry: false,
  })
  const data = tokenQuery.data ?? null
  const loading = !!token && tokenQuery.isLoading
  const error = !token
    ? 'No payment update token provided'
    : tokenQuery.error instanceof Error
      ? tokenQuery.error.message
      : tokenQuery.error
        ? 'This link has expired or is invalid'
        : ''

  // Update Payment Method click. The validate endpoint above only
  // resolves invoice context — it does not mint a Stripe Checkout
  // Session (those are billable + single-use, so the server defers
  // creation until the customer actually clicks). On click we POST
  // to /checkout, the server creates the session via the tenant's
  // connected Stripe account, and we redirect the browser to the
  // returned URL. Errors (Stripe not connected for this mode,
  // missing payment setup, etc.) surface inline so the customer
  // sees a specific reason instead of a dead button.
  const handleUpdate = async () => {
    if (!token || redirecting) return
    setSubmitError('')
    setRedirecting(true)
    try {
      const apiBase = import.meta.env.VITE_API_URL || ''
      const res = await fetch(
        `${apiBase}/v1/public/payment-updates/${encodeURIComponent(token)}/checkout`,
        { method: 'POST' },
      )
      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        throw new Error(body?.error?.message || 'We could not start the payment update. Please try again or contact your billing administrator.')
      }
      const { url } = await res.json() as { url?: string }
      if (!url) throw new Error('Payment update could not be started — no checkout URL returned.')
      window.location.href = url
    } catch (err) {
      setSubmitError(err instanceof Error ? err.message : 'Unexpected error starting the payment update.')
      setRedirecting(false)
    }
  }

  return (
    <div className="min-h-screen bg-background flex items-center justify-center p-4">
      <div className="w-full max-w-md">
        {/* Brand header — the TENANT's identity, not Velox: this is the
            payment-recovery funnel, and a customer arriving from a
            failed-payment email who sees an unknown brand treats the
            page as phishing (audit P13). Falls back to a neutral header
            when the tenant hasn't configured branding. */}
        <div className="text-center mb-8">
          {data?.branding?.logo_url ? (
            <img
              src={data.branding.logo_url}
              alt={data.branding.company_name || 'Company logo'}
              className="h-10 mx-auto mb-2 object-contain"
            />
          ) : (
            <h1
              className="text-2xl font-bold text-foreground"
              style={isHexColor(data?.branding?.brand_color) ? { color: data!.branding!.brand_color } : undefined}
            >
              {data?.branding?.company_name || 'Secure payment update'}
            </h1>
          )}
          <p className="text-sm text-muted-foreground mt-1">
            {data?.branding?.company_name ? 'Secure payment setup' : 'Add a payment method'}
          </p>
        </div>

        <Card className="overflow-hidden">
          <CardContent className="p-0">
            {loading ? (
              <div className="p-12 text-center">
                <Loader2 className="h-8 w-8 animate-spin text-primary mx-auto" />
                <p className="text-sm text-muted-foreground mt-4">Verifying your link...</p>
              </div>
            ) : error ? (
              <div className="p-8 text-center">
                <div className="w-12 h-12 rounded-full bg-destructive/10 flex items-center justify-center mx-auto mb-4">
                  <Clock size={24} className="text-destructive/60" />
                </div>
                <p className="text-sm font-medium text-foreground">Link expired or invalid</p>
                <p className="text-sm text-muted-foreground mt-2">{error}</p>
                <p className="text-xs text-muted-foreground mt-4">Please contact your billing provider for a new link.</p>
              </div>
            ) : data ? (
              <>
                {/* Alert banner — settled/annulled invoices state the
                    truth instead of asking for payment; adding a card is
                    still offered because it serves the NEXT invoice. */}
                {data.invoice_status === 'paid' || data.invoice_status === 'voided' ? (
                  <div className="bg-muted px-6 py-4 border-b border-border">
                    <div className="flex items-start gap-3">
                      <Info size={18} className="text-muted-foreground mt-0.5 shrink-0" />
                      <div>
                        <p className="text-sm font-medium text-foreground">
                          {data.invoice_status === 'paid'
                            ? 'This invoice is already paid'
                            : 'This invoice was canceled'}
                        </p>
                        <p className="text-xs text-muted-foreground mt-1">
                          {data.invoice_status === 'paid'
                            ? 'Nothing is due. You can still add a payment method so future invoices are collected automatically.'
                            : 'No payment is needed. You can still add a payment method so future invoices are collected automatically.'}
                        </p>
                      </div>
                    </div>
                  </div>
                ) : (
                <div className="bg-amber-50 dark:bg-amber-500/10 px-6 py-4 border-b border-amber-100 dark:border-amber-500/20">
                  <div className="flex items-start gap-3">
                    <AlertTriangle size={18} className="text-amber-500 mt-0.5 shrink-0" />
                    <div>
                      <p className="text-sm font-medium text-amber-800 dark:text-amber-400">Payment method needed</p>
                      <p className="text-xs text-amber-600 dark:text-amber-500 mt-1">
                        Add a payment method to pay this invoice — it takes a minute, and future invoices will be collected automatically.
                      </p>
                    </div>
                  </div>
                </div>
                )}

                {/* Invoice details */}
                <div className="p-6 space-y-4">
                  <div>
                    <p className="text-xs text-muted-foreground uppercase tracking-wider">Customer</p>
                    <p className="text-sm font-medium text-foreground mt-1">{data.customer_name}</p>
                  </div>

                  <div className="bg-muted rounded-xl p-4 space-y-3">
                    <div className="flex items-center justify-between">
                      <span className="text-sm text-muted-foreground">Invoice</span>
                      <span className="text-sm font-mono text-foreground">{data.invoice_number}</span>
                    </div>
                    {/* amount_due_cents survives a void/settle by design
                        (the transition reverses collection, it doesn't
                        rewrite the figure) — so label it for what it is
                        once nothing is owed, rather than dunning a
                        customer for a settled invoice. */}
                    <div className="flex items-center justify-between">
                      <span className="text-sm text-muted-foreground">
                        {data.invoice_status === 'paid' || data.invoice_status === 'voided'
                          ? 'Invoice amount'
                          : 'Amount Due'}
                      </span>
                      <span className="text-lg font-semibold text-foreground">{formatCents(data.amount_due_cents, data.currency)}</span>
                    </div>
                  </div>

                  <Button
                    onClick={handleUpdate}
                    disabled={redirecting}
                    className="w-full"
                    size="lg"
                  >
                    {redirecting ? (
                      'Redirecting to Stripe...'
                    ) : (
                      <>
                        <CreditCard size={16} className="mr-2" />
                        Add payment method
                        <ExternalLink size={14} className="ml-2 opacity-50" />
                      </>
                    )}
                  </Button>

                  {submitError && (
                    <div className="rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-xs text-destructive">
                      {submitError}
                    </div>
                  )}

                  <div className="flex items-center justify-center gap-1.5 text-xs text-muted-foreground">
                    <ShieldCheck size={12} />
                    <span>Secured by Stripe. Your card details are never stored on our servers.</span>
                  </div>
                </div>
              </>
            ) : null}
          </CardContent>
        </Card>

        <p className="text-xs text-muted-foreground text-center mt-6">
          Powered by Velox Billing
        </p>
      </div>
    </div>
  )
}
