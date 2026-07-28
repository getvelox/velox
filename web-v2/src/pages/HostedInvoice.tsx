import { useEffect, useMemo, useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useQuery } from '@tanstack/react-query'
import { useParams, useSearchParams } from 'react-router-dom'
import { formatCents, formatRate, formatDate, formatTaxRate } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import {
  CheckCircle2,
  CreditCard,
  Download,
  ExternalLink,
  Loader2,
  ShieldCheck,
  XCircle,
  Clock,
} from 'lucide-react'

// Shape mirrors internal/hostedinvoice/handler.go:hostedInvoicePayload.
// Kept in one place so a contract change on the server surfaces as a TS
// error here rather than silent runtime drift.
interface HostedInvoicePayload {
  invoice: {
    invoice_number: string
    status: 'finalized' | 'paid' | 'voided' | string
    payment_status: string
    currency: string
    subtotal_cents: number
    discount_cents: number
    tax_amount_cents: number
    tax_rate: number // ADR-042/043: percent rate (7.25 = 7.25%), 4-decimal precision
    tax_name?: string
    tax_reverse_charge?: boolean
    tax_exempt_reason?: string
    total_amount_cents: number
    amount_due_cents: number
    amount_paid_cents: number
    credits_applied_cents: number
    issued_at?: string
    due_at?: string
    paid_at?: string
    voided_at?: string
    // Invoice's billing timezone — this public page has no tenant-TZ context,
    // so civil-day dates above are rendered in this zone (falling back to UTC),
    // matching the PDF, not the viewer's browser zone (ADR-074/075).
    billing_timezone?: string
    memo?: string
    footer?: string
  }
  line_items: {
    description: string
    quantity: number
    // Exact fractional usage quantity (decimal string); when present it is
    // the display quantity — the integer `quantity` is its truncation.
    quantity_decimal?: string
    unit_amount_cents: number
    // Full-precision per-unit price in decimal cents (e.g. "0.3" = $0.003);
    // render with formatRate so sub-cent rates don't collapse to "$0.00".
    unit_amount_decimal?: string
    amount_cents: number
    tax_amount_cents?: number
    total_amount_cents: number
    currency: string
  }[]
  bill_to: {
    name?: string
    email?: string
    address_line1?: string
    address_line2?: string
    city?: string
    state?: string
    postal_code?: string
    country?: string
  }
  branding: {
    company_name?: string
    company_email?: string
    company_phone?: string
    company_address_line1?: string
    company_address_line2?: string
    company_city?: string
    company_state?: string
    company_postal_code?: string
    company_country?: string
    logo_url?: string
    brand_color?: string
    support_url?: string
  }
  pay_enabled: boolean
  livemode: boolean
}

const apiBase = import.meta.env.VITE_API_URL || ''

// isHexColor guards against tenants writing unexpected values into
// brand_color. The server already validates on save (^#[0-9a-f]{6}$) but
// the client re-checks before feeding the value into inline styles —
// defence in depth against any future migration drift.
function isHexColor(c: string | undefined): c is string {
  return !!c && /^#[0-9a-f]{6}$/i.test(c)
}

function formatAddress(parts: (string | undefined)[]): string[] {
  return parts.filter((p): p is string => !!p && p.trim() !== '')
}

export default function HostedInvoicePage() {
  const { token = '' } = useParams<{ token: string }>()
  const [searchParams] = useSearchParams()
  const paidSignal = searchParams.get('paid') === '1'

  const [payingStatus, setPayingStatus] = useState<'idle' | 'creating' | 'redirecting'>('idle')
  const [payError, setPayError] = useState<string | null>(null)

  // The redirect from Stripe Checkout beats the webhook that flips the
  // invoice to paid, so with ?paid=1 the page POLLS (3s) until the
  // payload's payment_status — the authoritative backend signal, no
  // client-side inference — turns terminal (succeeded or failed).
  // Capped at 3 minutes: async payment methods can legitimately take
  // longer, at which point the copy turns honest instead of spinning
  // forever. Pre-fix the page promised to "update automatically", never
  // refetched, and left the Pay button live — the direct feeder for
  // double-charged cards.
  const POLL_CAP_MS = 3 * 60 * 1000
  const [pollTimedOut, setPollTimedOut] = useState(false)
  useEffect(() => {
    if (!paidSignal) return
    const t = window.setTimeout(() => setPollTimedOut(true), POLL_CAP_MS)
    return () => window.clearTimeout(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [paidSignal])

  const isPaymentSettling = (d?: HostedInvoicePayload | null) =>
    !!d &&
    paidSignal &&
    d.invoice.status === 'finalized' &&
    d.invoice.payment_status !== 'succeeded' &&
    d.invoice.payment_status !== 'failed'

  // Invoice fetch keyed by (token, paidSignal). retry=false so a
  // genuinely invalid token doesn't pound the public endpoint; the
  // refetchInterval below handles the settle-poll instead.
  const invoiceQuery = useQuery({
    queryKey: ['public-invoice', token, paidSignal],
    queryFn: async (): Promise<HostedInvoicePayload> => {
      const res = await fetch(`${apiBase}/v1/public/invoices/${encodeURIComponent(token)}`)
      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        throw new Error(body?.error?.message || 'This invoice link has expired or is invalid.')
      }
      return (await res.json()) as HostedInvoicePayload
    },
    enabled: !!token,
    retry: false,
    refetchInterval: (query) => {
      if (pollTimedOut) return false
      return isPaymentSettling(query.state.data as HostedInvoicePayload | undefined) ? 3000 : false
    },
  })
  const data = invoiceQuery.data ?? null
  usePageTitle(data?.invoice?.invoice_number ?? 'Invoice')
  const loading = !!token && invoiceQuery.isLoading
  const error = !token
    ? 'Invoice link is missing.'
    : invoiceQuery.error instanceof Error
      ? invoiceQuery.error.message
      : invoiceQuery.error
        ? 'This invoice link has expired or is invalid.'
        : ''

  const brandColor = useMemo(() => {
    const c = data?.branding.brand_color
    return isHexColor(c) ? c : undefined
  }, [data])

  const handlePay = async () => {
    if (!data || !data.pay_enabled) return
    setPayError(null)
    setPayingStatus('creating')
    try {
      const res = await fetch(
        `${apiBase}/v1/public/invoices/${encodeURIComponent(token)}/checkout`,
        { method: 'POST' },
      )
      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        throw new Error(body?.error?.message || 'Unable to start payment. Please try again.')
      }
      const { url } = (await res.json()) as { url: string }
      setPayingStatus('redirecting')
      window.location.href = url
    } catch (err) {
      setPayingStatus('idle')
      setPayError(err instanceof Error ? err.message : 'Unable to start payment.')
    }
  }

  const pdfHref = `${apiBase}/v1/public/invoices/${encodeURIComponent(token)}/pdf`

  // ---- Loading / error shells ----

  if (loading) {
    return (
      <div className="min-h-screen bg-background flex items-center justify-center p-4">
        <div className="flex flex-col items-center gap-3 text-muted-foreground">
          <Loader2 className="h-6 w-6 animate-spin" aria-hidden="true" />
          <p className="text-sm">Loading invoice…</p>
        </div>
      </div>
    )
  }

  if (error || !data) {
    return (
      <div className="min-h-screen bg-background flex items-center justify-center p-4">
        <Card className="w-full max-w-md">
          <CardContent className="p-8 text-center space-y-4">
            <div className="w-12 h-12 rounded-full bg-destructive/10 flex items-center justify-center mx-auto">
              <Clock size={24} className="text-destructive/70" aria-hidden="true" />
            </div>
            <h1 className="text-base font-semibold text-foreground">Invoice unavailable</h1>
            <p className="text-sm text-muted-foreground">
              {error || 'This invoice link has expired or is invalid.'}
            </p>
            <p className="text-xs text-muted-foreground">
              Please contact your billing provider for a new link.
            </p>
          </CardContent>
        </Card>
      </div>
    )
  }

  const { invoice, line_items, bill_to, branding } = data
  const companyAddress = formatAddress([
    branding.company_address_line1,
    branding.company_address_line2,
    [branding.company_city, branding.company_state, branding.company_postal_code]
      .filter(Boolean)
      .join(', '),
    branding.company_country,
  ])
  const billToAddress = formatAddress([
    bill_to.address_line1,
    bill_to.address_line2,
    [bill_to.city, bill_to.state, bill_to.postal_code].filter(Boolean).join(', '),
    bill_to.country,
  ])

  // ---- Status banner ----
  const banner = (() => {
    if (invoice.status === 'paid') {
      return (
        <div
          role="status"
          className="rounded-lg bg-emerald-50 dark:bg-emerald-500/10 border border-emerald-200 dark:border-emerald-500/20 px-4 py-3 flex items-start gap-3"
        >
          <CheckCircle2 size={18} className="text-emerald-600 dark:text-emerald-400 mt-0.5 shrink-0" aria-hidden="true" />
          <div className="text-sm">
            <p className="font-medium text-emerald-800 dark:text-emerald-300">Paid</p>
            {invoice.paid_at && (
              <p className="text-emerald-700/80 dark:text-emerald-400/80">
                Received on {formatDate(invoice.paid_at, invoice.billing_timezone || "UTC")}
              </p>
            )}
          </div>
        </div>
      )
    }
    if (invoice.status === 'voided') {
      return (
        <div
          role="status"
          className="rounded-lg bg-muted px-4 py-3 flex items-start gap-3"
        >
          <XCircle size={18} className="text-muted-foreground mt-0.5 shrink-0" aria-hidden="true" />
          <div className="text-sm">
            <p className="font-medium text-foreground">Voided</p>
            {invoice.voided_at && (
              <p className="text-muted-foreground">
                Voided on {formatDate(invoice.voided_at, invoice.billing_timezone || "UTC")} — this invoice is no longer owed.
              </p>
            )}
          </div>
        </div>
      )
    }
    if (invoice.status === 'uncollectible') {
      // Collection is closed on this invoice, so there is no Pay button —
      // the dunning-escalation email links here, and without this banner
      // the customer lands on a dead end with no explanation.
      return (
        <div
          role="status"
          className="rounded-lg bg-muted px-4 py-3 flex items-start gap-3"
        >
          <XCircle size={18} className="text-muted-foreground mt-0.5 shrink-0" aria-hidden="true" />
          <div className="text-sm">
            <p className="font-medium text-foreground">This invoice is closed</p>
            <p className="text-muted-foreground">
              Online payment is no longer available for this invoice. Please
              contact support to settle it or restore service.
            </p>
          </div>
        </div>
      )
    }
    if (paidSignal && invoice.status === 'finalized' && invoice.payment_status === 'failed') {
      // Authoritative failure from the backend: the charge did not go
      // through. Re-enabling Pay is safe — no successful charge exists.
      return (
        <div
          role="alert"
          className="rounded-lg bg-red-50 dark:bg-red-500/10 border border-red-200 dark:border-red-500/20 px-4 py-3 flex items-start gap-3"
        >
          <XCircle size={18} className="text-red-600 dark:text-red-400 mt-0.5 shrink-0" aria-hidden="true" />
          <div className="text-sm">
            <p className="font-medium text-red-800 dark:text-red-300">Payment didn't complete</p>
            <p className="text-red-700/80 dark:text-red-400/80">
              Your card was not charged. You can try again below.
            </p>
          </div>
        </div>
      )
    }
    if (isPaymentSettling(data) && pollTimedOut) {
      // Async payment methods can take longer than our poll window.
      // Honest copy, and the Pay button STAYS hidden — with session
      // reuse a re-click returns the same Checkout session anyway, and
      // a live Pay button here is how cards get charged twice.
      return (
        <div
          role="status"
          className="rounded-lg bg-amber-50 dark:bg-amber-500/10 border border-amber-200 dark:border-amber-500/20 px-4 py-3 flex items-start gap-3"
        >
          <Loader2 size={18} className="text-amber-600 dark:text-amber-400 mt-0.5 shrink-0" aria-hidden="true" />
          <div className="text-sm">
            <p className="font-medium text-amber-800 dark:text-amber-300">Confirmation is taking longer than usual</p>
            <p className="text-amber-700/80 dark:text-amber-400/80">
              Your payment is still being confirmed — you will not be charged twice. A receipt email will follow once it completes.
            </p>
          </div>
        </div>
      )
    }
    if (isPaymentSettling(data)) {
      // Stripe Checkout redirected with ?paid=1 but the webhook hasn't
      // caught up yet. The page polls every 3s until payment_status
      // turns terminal.
      return (
        <div
          role="status"
          className="rounded-lg bg-emerald-50 dark:bg-emerald-500/10 border border-emerald-200 dark:border-emerald-500/20 px-4 py-3 flex items-start gap-3"
        >
          <Loader2 size={18} className="text-emerald-600 dark:text-emerald-400 mt-0.5 shrink-0 animate-spin" aria-hidden="true" />
          <div className="text-sm">
            <p className="font-medium text-emerald-800 dark:text-emerald-300">Processing your payment…</p>
            <p className="text-emerald-700/80 dark:text-emerald-400/80">
              This page will update automatically once confirmation arrives.
            </p>
          </div>
        </div>
      )
    }
    return null
  })()

  return (
    <div className="min-h-screen bg-muted/30">
      <div className="mx-auto w-full max-w-3xl p-4 sm:p-6 lg:p-8">
        {/* Header: tenant branding */}
        <header className="flex items-center gap-3 pb-6">
          {branding.logo_url ? (
            <img
              src={branding.logo_url}
              alt=""
              className="h-10 w-10 rounded object-contain bg-background ring-1 ring-border"
              onError={e => {
                // Hide broken logo so the layout doesn't keep the
                // empty slot. No fallback image (industry practice:
                // don't invent branding the tenant didn't provide).
                ;(e.target as HTMLImageElement).style.display = 'none'
              }}
            />
          ) : null}
          <div className="flex-1">
            <h1 className="text-lg font-semibold text-foreground leading-tight">
              {branding.company_name || 'Invoice'}
            </h1>
            {branding.support_url && (
              <a
                href={branding.support_url}
                className="text-xs text-muted-foreground hover:text-foreground inline-flex items-center gap-1"
                target="_blank"
                rel="noopener noreferrer"
              >
                Contact support <ExternalLink size={10} aria-hidden="true" />
              </a>
            )}
          </div>
        </header>

        {/* Optional tenant accent bar — only rendered if brand_color set */}
        {brandColor && (
          <div
            aria-hidden="true"
            className="h-1 w-full rounded-full mb-6"
            style={{ backgroundColor: brandColor }}
          />
        )}

        {/* Test-mode banner — an unmarked test invoice is indistinguishable
            from a real one to the customer it reaches (Stripe Checkout
            self-discloses test mode the same way). */}
        {data.livemode === false && (
          <div
            role="status"
            className="mb-4 rounded-lg bg-amber-50 dark:bg-amber-500/10 border border-amber-200 dark:border-amber-500/20 px-4 py-3"
          >
            <p className="text-sm font-medium text-amber-800 dark:text-amber-300">
              Test mode — this invoice is a test and no real payment will be collected.
            </p>
          </div>
        )}

        {/* Status banner */}
        {banner && <div className="mb-4">{banner}</div>}

        {/* Invoice card */}
        <Card>
          <CardContent className="p-6 sm:p-8 space-y-8">
            {/* Title row */}
            <div className="flex items-start justify-between gap-4 flex-wrap">
              <div>
                <p className="text-xs text-muted-foreground uppercase tracking-wider">Invoice</p>
                <h2 className="text-xl font-semibold text-foreground font-mono">
                  {invoice.invoice_number}
                </h2>
              </div>
              <div className="text-right">
                <p className="text-xs text-muted-foreground uppercase tracking-wider">Amount Due</p>
                <p className="text-2xl font-semibold text-foreground tabular-nums">
                  {formatCents(invoice.amount_due_cents, invoice.currency)}
                </p>
                {invoice.due_at && invoice.status === 'finalized' && (
                  <p className="text-xs text-muted-foreground mt-1">
                    Due {formatDate(invoice.due_at, invoice.billing_timezone || "UTC")}
                  </p>
                )}
              </div>
            </div>

            {/* Two-column meta: Bill to / From */}
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-6">
              <section>
                <h3 className="text-xs text-muted-foreground uppercase tracking-wider mb-2">
                  Bill to
                </h3>
                {bill_to.name && (
                  <p className="text-sm font-medium text-foreground">{bill_to.name}</p>
                )}
                {bill_to.email && (
                  <p className="text-sm text-muted-foreground">{bill_to.email}</p>
                )}
                {billToAddress.map((line, i) => (
                  <p key={i} className="text-sm text-muted-foreground">{line}</p>
                ))}
              </section>
              <section>
                <h3 className="text-xs text-muted-foreground uppercase tracking-wider mb-2">
                  From
                </h3>
                {branding.company_name && (
                  <p className="text-sm font-medium text-foreground">{branding.company_name}</p>
                )}
                {branding.company_email && (
                  <p className="text-sm text-muted-foreground">{branding.company_email}</p>
                )}
                {companyAddress.map((line, i) => (
                  <p key={i} className="text-sm text-muted-foreground">{line}</p>
                ))}
              </section>
            </div>

            {/* Line items */}
            <section aria-labelledby="line-items-heading">
              <h3 id="line-items-heading" className="sr-only">Line items</h3>
              <div className="overflow-hidden border rounded-lg">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="bg-muted/50 text-xs text-muted-foreground uppercase tracking-wider">
                      <th className="text-left px-4 py-2 font-medium">Description</th>
                      <th className="text-right px-4 py-2 font-medium">Qty</th>
                      <th className="text-right px-4 py-2 font-medium hidden sm:table-cell">Unit</th>
                      <th className="text-right px-4 py-2 font-medium">Amount</th>
                    </tr>
                  </thead>
                  <tbody>
                    {line_items.map((li, i) => (
                      <tr key={i} className="border-t">
                        <td className="px-4 py-3 text-foreground">{li.description}</td>
                        <td className="px-4 py-3 text-right text-muted-foreground tabular-nums">
                          {li.quantity_decimal && Number(li.quantity_decimal) !== 0
                            ? Number(li.quantity_decimal).toLocaleString(undefined, { maximumFractionDigits: 12 })
                            : li.quantity}
                        </td>
                        <td className="px-4 py-3 text-right text-muted-foreground tabular-nums hidden sm:table-cell">
                          {formatRate(li.unit_amount_decimal ?? li.unit_amount_cents, li.currency)}
                        </td>
                        <td className="px-4 py-3 text-right text-foreground tabular-nums">
                          {formatCents(li.amount_cents, li.currency)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </section>

            {/* Totals */}
            <section className="flex justify-end">
              <dl className="w-full sm:w-80 space-y-2 text-sm">
                <div className="flex justify-between">
                  <dt className="text-muted-foreground">Subtotal</dt>
                  <dd className="text-foreground tabular-nums">
                    {formatCents(invoice.subtotal_cents, invoice.currency)}
                  </dd>
                </div>
                {invoice.discount_cents > 0 && (
                  <div className="flex justify-between">
                    <dt className="text-muted-foreground">Discount</dt>
                    <dd className="text-foreground tabular-nums">
                      −{formatCents(invoice.discount_cents, invoice.currency)}
                    </dd>
                  </div>
                )}
                {invoice.tax_amount_cents > 0 && (
                  <div className="flex justify-between">
                    <dt className="text-muted-foreground">
                      {invoice.tax_name || 'Tax'}
                      {invoice.tax_rate > 0 && (
                        <span className="ml-1 text-xs">
                          ({formatTaxRate(invoice.tax_rate)}%)
                        </span>
                      )}
                    </dt>
                    <dd className="text-foreground tabular-nums">
                      {formatCents(invoice.tax_amount_cents, invoice.currency)}
                    </dd>
                  </div>
                )}
                {invoice.tax_reverse_charge && (
                  <p className="text-xs text-muted-foreground">
                    Reverse charge — customer self-assesses VAT/GST.
                  </p>
                )}
                {invoice.tax_exempt_reason && !invoice.tax_reverse_charge && (
                  <p className="text-xs text-muted-foreground">
                    Tax-exempt — {invoice.tax_exempt_reason}
                  </p>
                )}
                {invoice.credits_applied_cents > 0 && (
                  <div className="flex justify-between">
                    <dt className="text-muted-foreground">Credits applied</dt>
                    <dd className="text-foreground tabular-nums">
                      −{formatCents(invoice.credits_applied_cents, invoice.currency)}
                    </dd>
                  </div>
                )}
                <div className="flex justify-between pt-2 border-t font-semibold">
                  <dt className="text-foreground">Total</dt>
                  <dd className="text-foreground tabular-nums">
                    {formatCents(invoice.total_amount_cents, invoice.currency)}
                  </dd>
                </div>
                {invoice.amount_paid_cents > 0 && (
                  <div className="flex justify-between text-muted-foreground">
                    <dt>Paid</dt>
                    <dd className="tabular-nums">
                      {formatCents(invoice.amount_paid_cents, invoice.currency)}
                    </dd>
                  </div>
                )}
                {/* amount_due_cents survives a void / write-off by design —
                    those transitions reverse COLLECTION, they don't rewrite
                    the figure. Labelling it "Amount due" on a closed invoice
                    tells the customer they owe money we've stopped
                    collecting; name it for what it is instead (same fix as
                    the payment-update page, 2026-07-28). */}
                <div className="flex justify-between pt-2 border-t font-semibold">
                  <dt className="text-foreground">
                    {invoice.status === 'voided' || invoice.status === 'uncollectible'
                      ? 'Invoice amount'
                      : 'Amount due'}
                  </dt>
                  <dd className="text-foreground tabular-nums">
                    {formatCents(invoice.amount_due_cents, invoice.currency)}
                  </dd>
                </div>
              </dl>
            </section>

            {invoice.memo && (
              <section aria-labelledby="memo-heading" className="text-sm text-muted-foreground">
                <h3 id="memo-heading" className="sr-only">Memo</h3>
                <p className="whitespace-pre-wrap">{invoice.memo}</p>
              </section>
            )}

            {/* Actions. Pay is suppressed the whole time a redirect-back
                payment is settling (including past the poll cap) — a live
                Pay button during settle is how cards get charged twice.
                It reappears only on an authoritative payment_status of
                'failed' (no charge exists) or naturally when the invoice
                flips paid (pay_enabled goes false). */}
            <section className="flex flex-col sm:flex-row gap-3 pt-2">
              {data.pay_enabled && !isPaymentSettling(data) ? (
                <Button
                  onClick={handlePay}
                  disabled={payingStatus !== 'idle'}
                  size="lg"
                  className="flex-1"
                  // Inline style override so the Pay button carries the
                  // tenant's brand color when set. Falls through to the
                  // theme primary if brand_color is empty.
                  style={brandColor ? { backgroundColor: brandColor, borderColor: brandColor } : undefined}
                >
                  {payingStatus === 'creating' ? (
                    <>
                      <Loader2 size={16} className="mr-2 animate-spin" aria-hidden="true" />
                      Preparing checkout…
                    </>
                  ) : payingStatus === 'redirecting' ? (
                    <>
                      <Loader2 size={16} className="mr-2 animate-spin" aria-hidden="true" />
                      Redirecting to Stripe…
                    </>
                  ) : (
                    <>
                      <CreditCard size={16} className="mr-2" aria-hidden="true" />
                      Pay {formatCents(invoice.amount_due_cents, invoice.currency)}
                      <ExternalLink size={14} className="ml-2 opacity-60" aria-hidden="true" />
                    </>
                  )}
                </Button>
              ) : null}
              <Button
                render={<a href={pdfHref} target="_blank" rel="noopener noreferrer" />}
                variant="outline"
                size="lg"
                className={data.pay_enabled ? '' : 'flex-1'}
              >
                <Download size={16} className="mr-2" aria-hidden="true" />
                Download PDF
              </Button>
            </section>

            {payError && (
              <p className="mt-2 text-sm text-destructive" role="alert">{payError}</p>
            )}

            {data.pay_enabled && (
              <p className="text-xs text-muted-foreground flex items-center justify-center gap-1.5">
                <ShieldCheck size={12} aria-hidden="true" />
                Secured by Stripe. Your card details are never stored on our servers.
              </p>
            )}
          </CardContent>
        </Card>

        {invoice.footer && (
          <p className="text-xs text-muted-foreground text-center mt-6 whitespace-pre-wrap">
            {invoice.footer}
          </p>
        )}

        <p className="text-xs text-muted-foreground/70 text-center mt-4">
          Powered by Velox Billing
        </p>
      </div>
    </div>
  )
}
