import { setLastRequestId } from './lastRequestId'
// Recipe wire types are GENERATED from api/openapi.yaml (single source of
// truth) — the hand-written versions drifted from the server (`key` vs
// `recipe_key`, a phantom `products` role) until the 2026-07-26 recipe
// review. Type-only imports, so no runtime cycle with gen/ (whose client
// delegates back to apiRequest here).
import type { ListRecipes200 } from './gen/schemas/listRecipes200'
import type { RecipeDetail } from './gen/schemas/recipeDetail'
import type { RecipePreviewResult } from './gen/schemas/recipePreviewResult'
import type { RecipeInstance } from './gen/schemas/recipeInstance'

const API_BASE = '/v1'

// ApiError carries the structured pieces of a Velox error envelope so callers
// can route the message to the right UI surface. `field` identifies the single
// offending input (backed by the envelope's `param` slot); `code` is the
// stable business-rule code (e.g. billing_setup_incomplete) for switching on
// specific failure modes.
export class ApiError extends Error {
  status: number
  field?: string
  code?: string
  requestId?: string
  constructor(message: string, status: number, opts?: { field?: string; code?: string; requestId?: string }) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.field = opts?.field
    this.code = opts?.code
    this.requestId = opts?.requestId
  }
}

function humanizeError(msg: string): string {
  // "already exists: subscription code "acme-pro"" → "A subscription with code "acme-pro" already exists"
  const alreadyExists = msg.match(/already exists: (\w+) (?:code|with external_id|key) "(.+?)"/)
  if (alreadyExists) {
    const resource = alreadyExists[1].replace(/_/g, ' ')
    return `A ${resource} with that identifier ("${alreadyExists[2]}") already exists. Please use a different one.`
  }
  // "already exists: ..." generic
  if (msg.startsWith('already exists:')) {
    return 'This resource already exists. Please use a different identifier.'
  }
  // "not found" errors
  if (msg.includes('not found')) {
    return msg.replace(/not found/, 'could not be found')
  }
  return msg
}

// apiRequest is the shared fetch wrapper for all Velox API calls. The
// dashboard rides an httpOnly session cookie (minted via
// POST /v1/auth/login from the operator's email + password). The cookie
// attaches automatically via `credentials: 'include'` — no Authorization
// header, no credential in JS-reachable storage. SDK / curl callers
// reach the same endpoints with a Bearer header instead, which the
// server's MiddlewareOrAPIKey accepts as a fallback when the cookie is
// missing.
// DEFAULT_REQUEST_TIMEOUT_MS bounds every API call. A request that never
// receives a response — a stalled dev proxy, a reset/half-open connection, a
// wedged upstream — must NOT leave the UI spinning forever. We abort after
// this window so React Query surfaces a recoverable error instead of an
// infinite loading state. Set above the backend's 35s WriteTimeout (and its
// 30s per-route middleware timeout) so the server's own error wins when the
// backend is merely slow.
const DEFAULT_REQUEST_TIMEOUT_MS = 40_000

// fetchWithTimeout wraps fetch with an AbortController so a hung request fails
// fast instead of pending indefinitely. Used by every API + file call.
export async function fetchWithTimeout(url: string, init: RequestInit, timeoutMs = DEFAULT_REQUEST_TIMEOUT_MS): Promise<Response> {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    return await fetch(url, { ...init, signal: controller.signal })
  } catch (e) {
    if (e instanceof DOMException && e.name === 'AbortError') {
      throw new ApiError(
        `Request timed out after ${Math.round(timeoutMs / 1000)}s — the server didn't respond. Check the API is running and reachable, then retry.`,
        0,
        { code: 'timeout' },
      )
    }
    throw e
  } finally {
    clearTimeout(timer)
  }
}

export async function apiRequest<T>(method: string, path: string, body?: unknown, opts?: { idempotencyKey?: string }): Promise<T> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
  }
  if (opts?.idempotencyKey) {
    headers['Idempotency-Key'] = opts.idempotencyKey
  }

  const res = await fetchWithTimeout(`${API_BASE}${path}`, {
    method,
    headers,
    credentials: 'include',
    body: body ? JSON.stringify(body) : undefined,
  })

  // Record every observed request_id so "Report an issue" carries the most
  // recent trace handle even when the user's last request succeeded.
  const headerRequestId = res.headers.get('Velox-Request-Id') || ''
  if (headerRequestId) setLastRequestId(headerRequestId)

  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: { message: res.statusText } }))
    // Stripe-style envelope: { error: { type, code, message, param, request_id } }
    // param carries the offending field name for 422/409 responses. Older
    // string-shape errors ({ error: "..." }) fall back to the raw message.
    const detail = typeof err.error === 'object' ? err.error : null
    const raw = typeof err.error === 'string' ? err.error : (detail?.message || `HTTP ${res.status}`)
    const requestId = detail?.request_id || headerRequestId || undefined
    if (requestId) setLastRequestId(requestId)
    throw new ApiError(humanizeError(raw), res.status, {
      field: detail?.param || undefined,
      code: detail?.code || undefined,
      // Prefer request_id from the Stripe-style envelope; fall back to the
      // Velox-Request-Id header so errors that bypass the JSON writer (502s,
      // plain-text proxy errors) still carry a traceable ID to the toast.
      requestId,
    })
  }

  // 204 No Content / 205 Reset Content have no body — parsing as JSON would
  // throw SyntaxError and trip the caller's onError. Callers that use
  // apiRequest<void> expect undefined here.
  if (res.status === 204 || res.status === 205) {
    return undefined as T
  }
  return res.json()
}

export const api = {
  // Customers
  listCustomers: (params?: string) =>
    apiRequest<{ data: Customer[]; total: number }>('GET', `/customers${params ? '?' + params : ''}`),
  getCustomer: (id: string) =>
    apiRequest<Customer>('GET', `/customers/${id}`),
  createCustomer: (data: { external_id: string; display_name: string; email?: string; additional_emails?: string[]; test_clock_id?: string }) =>
    apiRequest<Customer>('POST', '/customers', data),
  rotateCostDashboardToken: (customerId: string) =>
    apiRequest<{ token: string; public_url: string }>('POST', `/customers/${customerId}/rotate-cost-dashboard-token`),
  listCustomerSentEmails: (customerId: string) =>
    apiRequest<{ sent_emails: SentEmail[] }>('GET', `/customers/${customerId}/sent-emails`),

  // Subscriptions
  listSubscriptions: (params?: string) =>
    apiRequest<{ data: Subscription[]; total: number }>('GET', `/subscriptions${params ? '?' + params : ''}`),
  createSubscription: (data: { code: string; display_name: string; customer_id: string; items: { plan_id: string; quantity?: number }[]; start_now?: boolean; billing_time?: string; trial_days?: number; usage_cap_units?: number | null; overage_action?: string; test_clock_id?: string }) =>
    apiRequest<Subscription>('POST', '/subscriptions', data),

  // Pricing
  listMeters: () =>
    apiRequest<{ data: Meter[] }>('GET', '/meters'),
  getMeter: (id: string) =>
    apiRequest<Meter>('GET', `/meters/${id}`),
  createMeter: (data: { key: string; name: string; unit?: string; aggregation?: string; rating_rule_version_id?: string }) =>
    apiRequest<Meter>('POST', '/meters', data),
  // "" clears the default binding; unmatched-dimension usage goes unbilled again.
  updateMeter: (id: string, data: { rating_rule_version_id: string }) =>
    apiRequest<Meter>('PATCH', `/meters/${id}`, data),
  listAllMeterPricingRules: () =>
    apiRequest<{ data: MeterPricingRule[] }>('GET', '/meter-pricing-rules'),
  listMeterPricingRules: (meterId: string) =>
    apiRequest<{ data: MeterPricingRule[] }>('GET', `/meters/${meterId}/pricing-rules`),
  createMeterPricingRule: (
    meterId: string,
    data: {
      rating_rule_version_id: string
      dimension_match: Record<string, string | number | boolean>
      aggregation_mode: MeterAggregationMode
      priority: number
    },
  ) =>
    apiRequest<MeterPricingRule>('POST', `/meters/${meterId}/pricing-rules`, data),
  deleteMeterPricingRule: (meterId: string, ruleId: string) =>
    apiRequest<{ status: string }>('DELETE', `/meters/${meterId}/pricing-rules/${ruleId}`),
  customerUsage: (customerId: string, params?: { from?: string; to?: string }) => {
    const qs = new URLSearchParams()
    if (params?.from) qs.set('from', params.from)
    if (params?.to) qs.set('to', params.to)
    const q = qs.toString()
    return apiRequest<CustomerUsage>('GET', `/customers/${customerId}/usage${q ? '?' + q : ''}`)
  },
  listPlans: () =>
    apiRequest<{ data: Plan[] }>('GET', '/plans'),
  getPlan: (id: string) =>
    apiRequest<Plan>('GET', `/plans/${id}`),
  createPlan: (data: { code: string; name: string; currency: string; billing_interval: string; base_amount_cents: number; base_bill_timing?: BillTiming; meter_ids?: string[] }) =>
    apiRequest<Plan>('POST', '/plans', data),
  updatePlan: (id: string, data: Partial<{ name: string; status: string; base_amount_cents: number; base_bill_timing: BillTiming; meter_ids: string[] }>) =>
    apiRequest<Plan>('PATCH', `/plans/${id}`, data),
  listRatingRules: () =>
    apiRequest<{ data: RatingRule[] }>('GET', '/rating-rules'),
  getRatingRule: (id: string) =>
    apiRequest<RatingRule>('GET', `/rating-rules/${id}`),
  createRatingRule: (data: { rule_key: string; name: string; mode: string; currency: string; flat_amount_cents?: string; graduated_tiers?: { up_to: number; unit_amount_cents: string }[]; package_size?: number; package_amount_cents?: number }) =>
    apiRequest<RatingRule>('POST', '/rating-rules', data),

  // Price overrides (ADR-070): negotiated per-customer prices, keyed by
  // rule_key so they survive version publishes. Create is an upsert — a
  // second override on the same (customer, rule) replaces the first from
  // the customer's next billing period. All rate changes (create, replace,
  // delete) take effect at the next period open, never mid-period.
  listPriceOverrides: (customerId?: string) =>
    apiRequest<{ data: PriceOverride[] }>('GET', `/price-overrides${customerId ? `?customer_id=${customerId}` : ''}`),
  createPriceOverride: (data: { customer_id: string; rating_rule_version_id: string; mode: string; flat_amount_cents?: string; graduated_tiers?: { up_to: number; unit_amount_cents: string }[]; package_size?: number; package_amount_cents?: number; reason?: string }) =>
    apiRequest<PriceOverride>('POST', '/price-overrides', data),
  deletePriceOverride: (id: string) =>
    apiRequest<void>('DELETE', `/price-overrides/${id}`),

  // Invoices
  listInvoices: (params?: string) =>
    apiRequest<{ data: Invoice[]; total: number }>('GET', `/invoices${params ? '?' + params : ''}`),
  getInvoice: (id: string) =>
    apiRequest<{ invoice: Invoice; line_items: LineItem[] }>('GET', `/invoices/${id}`),
  // Creates a draft invoice. Cycle invoices originate from the billing engine
  // and pass subscription_id; one-off invoices issued from the customer-page
  // composer omit it (backend allows null after migration 0060). Default
  // billing window is "now" when omitted, default net term is 30 days.
  createInvoice: (data: {
    customer_id: string
    subscription_id?: string
    currency?: string
    billing_period_start?: string
    billing_period_end?: string
    net_payment_term_days?: number
    memo?: string
    // line_items, when present, are created atomically with the invoice
    // header in a single request (no follow-up addInvoiceLineItem calls).
    line_items?: { description: string; line_type: string; quantity: number; unit_amount_cents: number }[]
  }) => apiRequest<Invoice>('POST', '/invoices', data),
  finalizeInvoice: (id: string) =>
    apiRequest<Invoice>('POST', `/invoices/${id}/finalize`),
  voidInvoice: (id: string) =>
    apiRequest<Invoice>('POST', `/invoices/${id}/void`),
  // markInvoiceUncollectible is the Stripe-parity bad-debt write-off.
  // Halts dunning, halts collection, leaves the invoice on the books.
  // Subscription is NOT auto-cancelled; that's a separate decision.
  // Transitionable forward to paid (via recordOfflinePayment) or void.
  markInvoiceUncollectible: (id: string) =>
    apiRequest<Invoice>('POST', `/invoices/${id}/mark-uncollectible`),
  // recordOfflineInvoicePayment marks an unpaid (or uncollectible)
  // invoice as paid based on out-of-band collection. Stripe-parity:
  // paid_out_of_band=true on /v1/invoices/{id}/pay. note is a free-
  // form operator memo (cheque number, wire ref, etc).
  recordOfflineInvoicePayment: (id: string, data: { note?: string }) =>
    apiRequest<Invoice>('POST', `/invoices/${id}/record-payment`, data),
  rotateInvoicePublicToken: (id: string) =>
    apiRequest<Invoice>('POST', `/invoices/${id}/rotate-public-token`),
  // retryInvoiceTax re-runs tax calculation against a draft invoice
  // currently in tax_status pending or failed. Backs the "Retry tax"
  // action surfaced by Attention. Returns the updated invoice with
  // its Attention re-derived — so the caller can render the new
  // banner state without a follow-up GET.
  retryInvoiceTax: (id: string) =>
    apiRequest<Invoice>('POST', `/invoices/${id}/retry-tax`),
  collectPayment: (id: string) =>
    apiRequest<Invoice>('POST', `/invoices/${id}/collect`),
  // additionalEmails tri-state (ADR-082): undefined → server CCs the
  // customer's stored additional_emails; [] → primary only; list →
  // exact override.
  sendInvoiceEmail: (invoiceId: string, email: string, additionalEmails?: string[]) =>
    apiRequest<{ status: string }>('POST', `/invoices/${invoiceId}/send`,
      additionalEmails === undefined ? { email } : { email, additional_emails: additionalEmails }),

  // Credit-note document email (ADR-082 rider) — same recipient contract.
  sendCreditNoteEmail: (creditNoteId: string, email: string, additionalEmails?: string[]) =>
    apiRequest<{ status: string }>('POST', `/credit-notes/${creditNoteId}/send`,
      additionalEmails === undefined ? { email } : { email, additional_emails: additionalEmails }),
  // Resends the payment-METHOD setup link email (Stripe Checkout in setup
  // mode) for a finalized, unpaid invoice with no card on file — the
  // no_payment_method attention card's "Resend setup link" nudge. Distinct
  // from sendInvoiceEmail, which emails the hosted-invoice "pay" link.
  resendSetupLink: (invoiceId: string) =>
    apiRequest<{ status: string }>('POST', `/invoices/${invoiceId}/resend-setup-link`, {}),
  getPaymentTimeline: (invoiceId: string) =>
    apiRequest<{ events: TimelineEvent[]; degraded: string[]; truncated: boolean }>('GET', `/invoices/${invoiceId}/payment-timeline`),
  getSubscriptionTimeline: (subscriptionId: string) =>
    apiRequest<{ events: TimelineEvent[]; truncated?: boolean }>('GET', `/subscriptions/${subscriptionId}/timeline`),

  // Legacy `setupPayment` (/checkout/setup) and `getPaymentStatus`
  // (/checkout/status) were removed in the unified-PM-paths cleanup.
  // All "add a payment method" flows now go through
  // `createCustomerSetupSession` / `sendCustomerSetupEmail` below.

  // Operator-side payment-method management (PCI SAQ-A; card capture
  // stays in Stripe-hosted iframe via setup-session, never on operator's
  // dashboard). Industry parity: Chargebee, Lago, Orb.
  listCustomerPaymentMethods: (customerId: string) =>
    apiRequest<{ data: CustomerPaymentMethod[]; total: number }>('GET', `/customers/${customerId}/payment-methods`),
  setDefaultCustomerPaymentMethod: (customerId: string, pmId: string) =>
    apiRequest<CustomerPaymentMethod>('POST', `/customers/${customerId}/payment-methods/${pmId}/default`),
  detachCustomerPaymentMethod: (customerId: string, pmId: string) =>
    apiRequest<CustomerPaymentMethod>('DELETE', `/customers/${customerId}/payment-methods/${pmId}`),
  createCustomerSetupSession: (customerId: string, returnUrl?: string) =>
    apiRequest<{ url: string; session_id: string }>('POST', `/customers/${customerId}/payment-methods/setup-session`, returnUrl ? { return_url: returnUrl } : {}),
  // Operator-initiated "send setup link via email" — primary path
  // (matches Stripe Send Payment Method + Chargebee Request Payment
  // Method). Mints the session and dispatches the email atomically;
  // the dashboard never sees the URL. Optional `note` is a free-form
  // operator message that prefaces the email body.
  sendCustomerSetupEmail: (customerId: string, note?: string) =>
    apiRequest<{ to: string; subject: string }>('POST', `/customers/${customerId}/payment-methods/send-setup-email`, note ? { note } : {}),

  // Credits
  listBalances: () =>
    apiRequest<{ data: CreditBalance[] }>('GET', '/credits/balances'),
  getBalance: (customerId: string) =>
    apiRequest<CreditBalance>('GET', `/credits/balance/${customerId}`),
  // Burndown: per-grant granted/drawn/remaining + kind subtotals — the
  // commit-drawdown answer without reconstructing it from the raw ledger.
  listCreditGrants: (customerId: string, includeExhausted = false) =>
    apiRequest<CreditGrantsResponse>('GET', `/credits/grants/${customerId}${includeExhausted ? '?include_exhausted=true' : ''}`),
  grantCredits: (data: { customer_id: string; amount_cents: number; description: string; expires_at?: string; grant_kind?: 'promotional' }, idempotencyKey?: string) =>
    apiRequest<CreditLedgerEntry>('POST', '/credits/grant', data, { idempotencyKey }),
  adjustCredits: (data: { customer_id: string; amount_cents: number; description: string }, idempotencyKey?: string) =>
    apiRequest<CreditLedgerEntry>('POST', '/credits/adjust', data, { idempotencyKey }),
  listLedger: (customerId: string, params?: { entry_type?: string; limit?: number; offset?: number; sort?: string; dir?: string }) => {
    const qs = new URLSearchParams()
    if (params?.entry_type) qs.set('entry_type', params.entry_type)
    if (params?.limit) qs.set('limit', String(params.limit))
    if (params?.offset) qs.set('offset', String(params.offset))
    if (params?.sort) qs.set('sort', params.sort)
    if (params?.dir) qs.set('dir', params.dir)
    const q = qs.toString()
    return apiRequest<{ data: CreditLedgerEntry[]; total: number }>('GET', `/credits/ledger/${customerId}${q ? '?' + q : ''}`)
  },

  // Customer portal
  customerOverview: (customerId: string) =>
    apiRequest<CustomerOverview>('GET', `/customer-portal/${customerId}/overview`),

  // Usage
  usageSummary: (customerId: string, from?: string, to?: string) => {
    const params = new URLSearchParams()
    if (from) params.set('from', from)
    if (to) params.set('to', to)
    const qs = params.toString()
    return apiRequest<UsageSummary>('GET', `/usage-summary/${customerId}${qs ? '?' + qs : ''}`)
  },
  listUsageEvents: (params?: string) =>
    apiRequest<{ data: UsageEvent[]; total: number }>('GET', `/usage-events${params ? '?' + params : ''}`),
  // Server-side aggregate that powers the /usage page's stat cards +
  // "Usage by Meter" breakdown. Same filter qs as listUsageEvents (drop
  // limit/offset — the whole point is the unbounded total). All numeric
  // fields except total_events arrive as decimal strings (NUMERIC(38,12)
  // per ADR-005) so fractional GPU-hours and partial tokens round-trip
  // without precision loss; coerce via Number()/parseFloat() for display
  // only. See internal/usage/store.go (Aggregate type).
  aggregateUsageEvents: (params?: string) =>
    apiRequest<UsageEventsAggregate>(
      'GET',
      `/usage-events/aggregate${params ? '?' + params : ''}`,
    ),

  // Customer updates
  updateCustomer: (id: string, data: { display_name?: string; email?: string; additional_emails?: string[]; dunning_policy_id?: string; status?: 'active' | 'archived' }) =>
    apiRequest<Customer>('PATCH', `/customers/${id}`, data),

  // Subscription detail
  getSubscription: (id: string) =>
    apiRequest<Subscription>('GET', `/subscriptions/${id}`),
  activateSubscription: (id: string) =>
    apiRequest<Subscription>('POST', `/subscriptions/${id}/activate`),
  cancelSubscription: (id: string) =>
    apiRequest<Subscription>('POST', `/subscriptions/${id}/cancel`),
  scheduleSubscriptionCancel: (id: string, body: { at_period_end: true } | { cancel_at: string }) =>
    apiRequest<Subscription>('POST', `/subscriptions/${id}/schedule-cancel`, body),
  clearScheduledSubscriptionCancel: (id: string) =>
    apiRequest<Subscription>('DELETE', `/subscriptions/${id}/scheduled-cancel`),
  pauseSubscriptionCollection: (id: string, body: { behavior: 'keep_as_draft'; resumes_at?: string }) =>
    apiRequest<Subscription>('PUT', `/subscriptions/${id}/pause-collection`, body),
  resumeSubscriptionCollection: (id: string) =>
    apiRequest<Subscription>('DELETE', `/subscriptions/${id}/pause-collection`),
  // Billing thresholds — Stripe-parity hard-cap config. PUT is replace-all
  // (omitted item_thresholds rows are deleted by the backend store);
  // DELETE removes the configuration entirely. The body must contain at
  // least one of amount_gte or item_thresholds[] — a body with neither is
  // rejected as a no-op masquerade. usage_gte arrives over the wire as a
  // decimal-string per ADR-005 to round-trip fractional meter quantities.
  setSubscriptionBillingThresholds: (id: string, body: {
    amount_gte?: number
    reset_billing_cycle?: boolean
    item_thresholds?: { subscription_item_id: string; usage_gte: string }[]
  }) =>
    apiRequest<Subscription>('PUT', `/subscriptions/${id}/billing-thresholds`, body),
  clearSubscriptionBillingThresholds: (id: string) =>
    apiRequest<Subscription>('DELETE', `/subscriptions/${id}/billing-thresholds`),
  endSubscriptionTrial: (id: string) =>
    apiRequest<Subscription>('POST', `/subscriptions/${id}/end-trial`),
  extendSubscriptionTrial: (id: string, body: { trial_end: string }) =>
    apiRequest<Subscription>('POST', `/subscriptions/${id}/extend-trial`, body),
  // Item CRUD. PATCH body is either `{quantity}` or `{new_plan_id, immediate}`,
  // never both — mirrors the backend's UpdateItemInput guard. Both mutations
  // take an Idempotency-Key (same contract as the credits calls): item
  // changes fire prorations and — for cross-interval swaps — refund credit
  // notes, so a network-level retry must replay the original response, not
  // re-run the money effects.
  addSubscriptionItem: (id: string, data: { plan_id: string; quantity?: number }, idempotencyKey?: string) =>
    apiRequest<SubscriptionItem>('POST', `/subscriptions/${id}/items`, data, { idempotencyKey }),
  updateSubscriptionItem: (id: string, itemID: string, data: { quantity?: number; new_plan_id?: string; immediate?: boolean }, idempotencyKey?: string) =>
    apiRequest<ItemChangeResult>('PATCH', `/subscriptions/${id}/items/${itemID}`, data, { idempotencyKey }),
  removeSubscriptionItem: (id: string, itemID: string) =>
    apiRequest<{ status: string }>('DELETE', `/subscriptions/${id}/items/${itemID}`),
  cancelPendingItemChange: (id: string, itemID: string) =>
    apiRequest<SubscriptionItem>('DELETE', `/subscriptions/${id}/items/${itemID}/pending-change`),
  invoicePreview: (subscriptionId: string) =>
    apiRequest<InvoicePreview>('GET', `/billing/preview/${subscriptionId}`),
  // Stripe Tier 1 parity for Invoice.upcoming. Same shape as the debug
  // route above; composes against customer + subscription + period.
  // Pass {customer_id} and the server picks the primary active sub +
  // current cycle. Pass {customer_id, subscription_id} to preview a
  // specific sub. Pass {customer_id, period: {from, to}} to preview an
  // explicit window. See docs/design-create-preview.md.
  createInvoicePreview: (data: { customer_id: string; subscription_id?: string; period?: { from: string; to: string } }) =>
    apiRequest<InvoicePreview>('POST', '/invoices/create_preview', data),

  // Billing profile
  getBillingProfile: (customerId: string) =>
    apiRequest<BillingProfile>('GET', `/customers/${customerId}/billing-profile`),
  upsertBillingProfile: (customerId: string, data: Partial<BillingProfile>) =>
    apiRequest<BillingProfile>('PUT', `/customers/${customerId}/billing-profile`, data),

  // Settings
  getSettings: () =>
    apiRequest<TenantSettings>('GET', '/settings'),
  updateSettings: (data: Partial<TenantSettings>) =>
    apiRequest<TenantSettings>('PUT', '/settings', data),

  // Dunning
  // Dunning policies (campaigns model, ADR-036). Multi-policy-per-
  // tenant with exactly one is_default. Customer assignment flows
  // through the customer Update endpoint (PATCH /customers/{id} body
  // carries dunning_policy_id).
  listDunningPolicies: () => apiRequest<{ data: DunningPolicyWithCount[] }>('GET', '/dunning/policies'),
  getDunningPolicy: (id: string) => apiRequest<DunningPolicy>('GET', `/dunning/policies/${id}`),
  createDunningPolicy: (data: Partial<DunningPolicy>) => apiRequest<DunningPolicy>('POST', '/dunning/policies', data),
  updateDunningPolicy: (id: string, data: Partial<DunningPolicy>) => apiRequest<DunningPolicy>('PATCH', `/dunning/policies/${id}`, data),
  deleteDunningPolicy: (id: string) => apiRequest<{ status: string }>('DELETE', `/dunning/policies/${id}`),
  setDefaultDunningPolicy: (id: string) => apiRequest<{ status: string }>('POST', `/dunning/policies/${id}/set-default`),
  listDunningRuns: (params?: string) => apiRequest<{ data: DunningRun[]; total: number }>('GET', `/dunning/runs${params ? '?' + params : ''}`),
  getDunningRun: (id: string) => apiRequest<{ run: DunningRun; events: DunningEvent[] }>('GET', `/dunning/runs/${id}`),
  // Aggregate counts + at-risk sum for the dashboard stat cards.
  // Server-side query so the cards stay accurate regardless of how
  // many runs exist (the paginated /runs list can't be trusted as
  // the source for counts).
  getDunningStats: () => apiRequest<DunningStats>('GET', '/dunning/stats'),
  resolveDunningRun: (id: string, resolution: string) => apiRequest<DunningRun>('POST', `/dunning/runs/${id}/resolve`, { resolution }),

  // Credit Notes
  listCreditNotes: (params?: string) => apiRequest<{ data: CreditNote[] }>('GET', `/credit-notes${params ? '?' + params : ''}`),
  createCreditNote: (data: {
    invoice_id: string
    reason: string
    refund_amount_cents?: number
    credit_amount_cents?: number
    out_of_band_amount_cents?: number
    refund_type?: string
    auto_issue?: boolean
    lines: { description: string; quantity: number; unit_amount_cents: number }[]
  }) => apiRequest<CreditNote>('POST', '/credit-notes', data),
  issueCreditNote: (id: string) => apiRequest<CreditNote>('POST', `/credit-notes/${id}/issue`),
  voidCreditNote: (id: string) => apiRequest<CreditNote>('POST', `/credit-notes/${id}/void`),
  retryCreditNoteRefund: (id: string) => apiRequest<CreditNote>('POST', `/credit-notes/${id}/retry-refund`),

  // Recipes (pricing templates) — see docs/design-recipes.md
  listRecipes: () => apiRequest<ListRecipes200>('GET', '/recipes'),
  getRecipe: (key: string) => apiRequest<RecipeDetail>('GET', `/recipes/${key}`),
  previewRecipe: (key: string, overrides: Record<string, string | number>) =>
    apiRequest<RecipePreviewResult>('POST', `/recipes/${key}/preview`, { overrides }),
  instantiateRecipe: (data: {
    key: string
    overrides?: Record<string, string | number>
  }) => apiRequest<RecipeInstance>('POST', `/recipes/${data.key}/instantiate`, { overrides: data.overrides }),

  // Audit Log
  // Response shape depends on pagination mode: offset (?offset=) returns
  // { data, total }; cursor (?after=) returns { data, has_more, next_cursor }
  // and skips the expensive COUNT. The declared type is the union of both.
  listAuditLog: (params?: string) => apiRequest<{ data: AuditEntry[]; total?: number; has_more?: boolean; next_cursor?: string }>('GET', `/audit-log${params ? '?' + params : ''}`),
  getAuditFilters: () => apiRequest<AuditFilterOptions>('GET', '/audit-log/filters'),

  // Webhooks
  listWebhookEndpoints: () => apiRequest<{ data: WebhookEndpoint[] }>('GET', '/webhook-endpoints/endpoints'),
  createWebhookEndpoint: (data: { url: string; description?: string; events?: string[] }) => apiRequest<{ endpoint: WebhookEndpoint; secret: string }>('POST', '/webhook-endpoints/endpoints', data),
  // PATCH mutates url/description/events/active WITHOUT rotating the
  // signing secret (delete+recreate was the only mutation path before,
  // forcing a receiver redeploy). Also how recipe-created endpoints
  // (inactive + placeholder URL) get pointed at a real receiver.
  updateWebhookEndpoint: (id: string, data: { url?: string; description?: string; events?: string[]; active?: boolean }) =>
    apiRequest<WebhookEndpoint>('PATCH', `/webhook-endpoints/endpoints/${id}`, data),
  deleteWebhookEndpoint: (id: string) => apiRequest<{ status: string }>('DELETE', `/webhook-endpoints/endpoints/${id}`),
  rotateWebhookSecret: (id: string) => apiRequest<{ secret: string; secondary_valid_until?: string }>('POST', `/webhook-endpoints/endpoints/${id}/rotate-secret`),
  getWebhookEndpointStats: () => apiRequest<{ data: { endpoint_id: string; total_deliveries: number; succeeded: number; failed: number; success_rate: number }[] }>('GET', '/webhook-endpoints/endpoints/stats'),
  listWebhookEvents: () => apiRequest<{ data: WebhookEvent[] }>('GET', '/webhook-endpoints/events'),
  replayWebhookEvent: (id: string) => apiRequest<{ status: string }>('POST', `/webhook-endpoints/events/${id}/replay`),

  // Week 6 real-time event UI. Replay returns the freshly-cloned event so
  // the UI can highlight it in the live tail; deliveries-list returns the
  // full per-attempt timeline for the expand-row diff view. The SSE stream
  // is NOT wired here — EventSource is opened directly from the page
  // because cookies ride automatically on same-origin requests and the
  // wrapper would only complicate the close/reconnect lifecycle.
  replayWebhookEventV2: (id: string) =>
    apiRequest<{ event_id: string; replay_of: string; status: string }>(
      'POST',
      `/webhook_events/${id}/replay`,
    ),
  listWebhookDeliveries: (id: string) =>
    apiRequest<WebhookDeliveriesResponse>(
      'GET',
      `/webhook_events/${id}/deliveries`,
    ),

  // Analytics
  addInvoiceLineItem: (invoiceId: string, data: { description: string; line_type: string; quantity: number; unit_amount_cents: number }) =>
    apiRequest<LineItem>('POST', `/invoices/${invoiceId}/line-items`, data),

  getAnalyticsOverview: (period?: string) =>
    apiRequest<AnalyticsOverview>('GET', `/analytics/overview${period ? `?period=${period}` : ''}`),
  getRevenueChart: (period: string) =>
    apiRequest<{ period: string; data: RevenueDataPoint[] }>('GET', `/analytics/revenue-chart?period=${period}`),
  getMRRMovement: (period: string) =>
    apiRequest<MRRMovementResponse>('GET', `/analytics/mrr-movement?period=${period}`),
  getUsageAnalytics: (period: string) =>
    apiRequest<UsageAnalyticsResponse>('GET', `/analytics/usage?period=${period}`),

  // Test clocks (test mode only — server returns 403 in live mode)
  listTestClocks: () => apiRequest<{ data: TestClock[] }>('GET', '/test-clocks'),
  getTestClock: (id: string) => apiRequest<TestClock>('GET', `/test-clocks/${id}`),
  createTestClock: (data: { name: string; frozen_time: string }) =>
    apiRequest<TestClock>('POST', '/test-clocks', data),
  advanceTestClock: (id: string, frozen_time: string) =>
    apiRequest<TestClock>('POST', `/test-clocks/${id}/advance`, { frozen_time }),
  retryAdvanceTestClock: (id: string) =>
    apiRequest<TestClock>('POST', `/test-clocks/${id}/retry-advance`),
  deleteTestClock: (id: string) =>
    apiRequest<{ status: string }>('DELETE', `/test-clocks/${id}`),
  listAttachedCustomers: (id: string) =>
    apiRequest<{ data: Customer[] }>('GET', `/test-clocks/${id}/customers`),
  listSubscriptionsOnClock: (id: string) =>
    apiRequest<{ data: Subscription[] }>('GET', `/test-clocks/${id}/subscriptions`),

  // API Keys
  listApiKeys: () => apiRequest<{ data: ApiKeyInfo[] }>('GET', '/api-keys'),
  createApiKey: (data: { name: string; key_type: string; expires_at?: string }) => apiRequest<{ key: ApiKeyInfo; raw_key: string }>('POST', '/api-keys', data),
  revokeApiKey: (id: string) => apiRequest<ApiKeyInfo>('DELETE', `/api-keys/${id}`),
  rotateApiKey: (id: string, expires_in_seconds?: number) =>
    apiRequest<{ old_key: ApiKeyInfo; new_key: ApiKeyInfo; raw_key: string }>(
      'POST',
      `/api-keys/${id}/rotate`,
      expires_in_seconds !== undefined ? { expires_in_seconds } : undefined,
    ),

  // Stripe credentials (per-tenant). Secrets post once here; the server keeps
  // them encrypted and only returns last4 + verify status. Deleting a row
  // revokes that mode entirely.
  listStripeCredentials: () => apiRequest<{ data: StripeProviderCredentials[] }>('GET', '/settings/stripe'),
  connectStripe: (data: { livemode: boolean; secret_key: string; publishable_key: string; webhook_secret?: string }) =>
    apiRequest<StripeProviderCredentials>('POST', '/settings/stripe', data),
  deleteStripeCredentials: (mode: 'live' | 'test') =>
    apiRequest<void>('DELETE', `/settings/stripe/${mode}`),
  // Second half of two-step setup: tenant connects API keys first, registers
  // the webhook endpoint in Stripe, then returns with the whsec_ secret.
  // Updates only the webhook secret on an existing row — no re-verify of the
  // API key, no need to re-paste secret/publishable.
  setStripeWebhookSecret: (mode: 'live' | 'test', webhook_secret: string) =>
    apiRequest<StripeProviderCredentials>('PATCH', `/settings/stripe/${mode}/webhook`, { webhook_secret }),

}

// Types
// SentEmail mirrors a single email_outbox row surfaced on the customer
// detail page's "Sent emails" section (Stripe shape). 30-day window,
// newest first, invoice-scoped customer-facing email types only
// (invoice / payment_receipt / payment_failed / payment_setup_request
// / dunning_warning / dunning_escalation).
export interface SentEmail {
  id: string
  email_type: string
  recipient: string
  status: 'pending' | 'dispatched' | 'failed' | 'skipped' | string
  // Provider-confirmed outcome layered over status (ADR-098):
  // 'dispatched' means the relay accepted the handoff; delivery_state is
  // what the provider then reported via webhook.
  delivery_state: 'unknown' | 'delivered' | 'bounced' | 'complained' | string
  invoice_number?: string
  last_error?: string
  created_at: string
  dispatched_at?: string
}

export interface Customer {
  id: string
  external_id: string
  display_name: string
  email: string
  // CC'd on billing emails (invoices, receipts, credit notes,
  // payment-issue notices). ADR-082.
  additional_emails?: string[]
  status: string
  created_at: string
  // Deliverability signal populated by the email bounce-capture hook
  // (T0-20). Absent on cold tenants; defaults to 'unknown' server-side.
  email_status?: 'unknown' | 'ok' | 'bounced' | 'complained'
  email_last_bounced_at?: string
  email_bounce_reason?: string
  // Customer-level test-clock attach (ADR-027, Stripe parity). Set at
  // create time only; once attached, every Subscription / Invoice for
  // this customer runs on the clock's simulated time. Empty for
  // live-mode customers and test-mode customers not pinned.
  test_clock_id?: string
  // DunningPolicyID assigns this customer to a specific dunning
  // policy (ADR-036 campaigns model). Empty/undefined = use the
  // tenant default. Updatable via PATCH /customers/{id}.
  dunning_policy_id?: string
}

export interface SubscriptionItem {
  id: string
  subscription_id: string
  plan_id: string
  quantity: number
  pending_plan_id?: string
  pending_plan_effective_at?: string
  // plan_changed_at stamps the last immediate plan swap on this item. Feeds
  // the per-item proration dedup key on the backend.
  plan_changed_at?: string
  created_at: string
  updated_at: string
}

export interface ProrationDetail {
  old_plan_id: string
  new_plan_id: string
  proration_factor: number
  amount_cents: number
  // 'invoice' = a proration charge invoice; 'credit' = a balance credit;
  // 'adjustment' = reduced an unpaid open invoice's amount due (ADR-050
  // unpaid-source path — NOT a refundable balance credit).
  type: 'invoice' | 'credit' | 'adjustment'
  invoice_id?: string
}

export interface ItemChangeResult {
  item: SubscriptionItem
  effective_at: string
  proration?: ProrationDetail
}

// SubscriptionItemThreshold is one per-item usage cap on a subscription.
// usage_gte is a decimal-string (e.g. "1000000.000000000000") so meter
// quantities that can be fractional round-trip without float drift.
export interface SubscriptionItemThreshold {
  subscription_item_id: string
  usage_gte: string
}

// BillingThresholds is the Stripe-parity hard-cap config attached to a
// subscription. Either amount_gte alone, item_thresholds[] alone, or both.
// item_thresholds is always-array (the dashboard's .map() works in all
// cases). amount_gte is integer cents in the subscription's currency;
// multi-currency subs reject amount_gte at PUT time.
export interface BillingThresholds {
  amount_gte?: number
  reset_billing_cycle: boolean
  item_thresholds: SubscriptionItemThreshold[]
}

export interface TestClock {
  id: string
  tenant_id: string
  livemode: boolean
  name: string
  frozen_time: string
  status: 'ready' | 'advancing' | 'internal_failure'
  created_at: string
  updated_at: string
  deletes_after?: string | null
  // last_failure_reason is set when status='internal_failure' to
  // explain the prior catchup error. Cleared on retry success or
  // a fresh advance. ADR-018.
  last_failure_reason?: string | null
  // last_advance_summary records what the most recent advance produced.
  // Null until the clock has been advanced with billing wired; rendered as
  // the "Last advance results" card once status is back to 'ready'.
  last_advance_summary?: AdvanceSummary | null
}

// AdvanceSummary mirrors domain.AdvanceSummary: the per-phase counts a
// test-clock advance produced, plus the simulated span it covered.
export interface AdvanceSummary {
  advanced_from: string
  advanced_to: string
  invoices_generated: number
  trials_activated: number
  pauses_resumed: number
  thresholds_fired: number
  tax_retried: number
  charges_retried: number
  clawbacks_issued: number
  credits_expired: number
  dunning_advanced: number
  had_errors: boolean
}

export interface Subscription {
  id: string
  code: string
  display_name: string
  customer_id: string
  test_clock_id?: string
  // items is the authoritative list of priced lines on the subscription.
  // Present on all list/get responses from FEAT-5 onward. Pre-FEAT-5 code
  // paths may omit it, so treat as optional and fall back gracefully.
  items?: SubscriptionItem[]
  status: string
  billing_time: string
  current_billing_period_start?: string
  current_billing_period_end?: string
  // Backend-authored inclusive period range ("Jun 1 – Jun 30"), rendered in the
  // org billing TZ (ADR-077). Use this verbatim for period-range labels — do
  // NOT recompute from the half-open start/end, which would duplicate the Go
  // inclusive-day logic. Subscriptions carry no per-sub timezone; a sub renders
  // in the one org timezone (= the live tenant setting), so auxiliary date
  // labels can pass undefined to formatCivil* and fall back to the tenant TZ.
  current_billing_period_display?: string
  next_billing_at?: string
  trial_start_at?: string
  trial_end_at?: string
  usage_cap_units?: number | null
  overage_action?: string
  cancel_at_period_end?: boolean
  // Derived, read-only: the backend's authoritative "when does it actually
  // cancel" (trialing+flag → trial end; active+flag → period end; explicit
  // cancel_at otherwise). Consumers must not re-derive from the flag.
  cancel_effective_at?: string
  cancel_at?: string
  canceled_at?: string
  pause_collection?: {
    behavior: 'keep_as_draft'
    resumes_at?: string
  }
  // billing_thresholds is the Stripe-parity hard-cap config. Nil/missing
  // means no cap is configured. When the running cycle subtotal crosses
  // amount_gte, or any item's running quantity crosses its usage_gte, the
  // billing engine fires an early finalize.
  billing_thresholds?: BillingThresholds
  created_at: string
}

// Attention surface — the unified "this invoice needs operator
// attention" payload computed server-side. See ADR-009. Field shapes
// mirror the Go domain.Attention struct one-for-one.
export type AttentionSeverity = 'info' | 'warning' | 'critical'

export type AttentionReason =
  | 'tax_calculation_failed'
  | 'tax_location_required'
  | 'payment_failed'
  | 'payment_unconfirmed'
  // payment_anomaly was shipped to the Go enum and the OpenAPI spec (ADR-068)
  // but never added here — this hand-maintained mirror had silently drifted
  // from the contract it claims to mirror one-for-one.
  | 'payment_anomaly'
  | 'payment_processing'
  | 'payment_scheduled'
  | 'awaiting_payment'
  | 'no_payment_method'
  | 'dunning_exhausted'
  | 'collection_paused'
  | 'commit_exposure'

export type AttentionAction =
  | 'edit_billing_profile'
  | 'retry_tax'
  | 'retry_payment'
  | 'wait_provider'
  | 'rotate_api_key'
  | 'reconcile_payment'
  | 'review_registration'
  | 'charge_now'
  | 'send_reminder'
  | 'add_payment_method'
  | 'update_payment_method'
  | 'connect_tax_provider'
  | 'void_invoice'

export interface AttentionActionItem {
  code: AttentionAction
  label?: string
}

// Why a WRITTEN-OFF invoice cannot be charged. Computed server-side and
// published so the dashboard disables "Charge customer" WITH its reason,
// instead of letting an operator confirm a charge the server will refuse.
// Absent on every invoice that is not uncollectible, and on recoverable ones.
export interface RecoveryBlock {
  blocked: boolean
  code?: string
  message?: string
}

export interface InvoiceAttention {
  severity: AttentionSeverity
  reason: AttentionReason
  message: string
  actions?: AttentionActionItem[]
  // Open dotted code (e.g. "tax.customer_data_invalid"). Stable but
  // extensible — new codes ship without contract bump.
  code?: string
  doc_url?: string
  // Dotted-path pointer at the field needing edit (Stripe parity).
  // E.g. "customer.address.postal_code" for tax_location_required.
  param?: string
  // Velox's own classification context — operator-safe, our framing.
  // Disclosed under "Detail" when present. Empty when the headline +
  // typed code + actions cover everything an operator needs.
  detail?: string
  // Raw upstream payload from a third-party provider (Stripe Tax JSON
  // envelope, Stripe last_payment_error body). Disclosed under
  // "Provider response". Populated ONLY when we actually called the
  // provider and received a response — pre-flight classification
  // errors (e.g. tax.provider_not_configured) leave this empty.
  // ADR-025.
  provider_response?: string
  // ISO timestamp marking when the condition started; operators triage
  // by age.
  since?: string
  // ISO timestamp of the next scheduled engine action — only set when
  // there's a real automatic retry queued (e.g. auto_charge_pending
  // sweep, dunning retry). Crucially NOT due_at — due_at is a
  // deadline, not an engine action. Empty when no automatic next
  // action exists (e.g. no_payment_method: operator must act).
  next_attempt_at?: string
  // ISO timestamp of the invoice's payment deadline (mirrors
  // invoice.due_at). Distinct from next_attempt_at — the engine does
  // NOT auto-act on this date; it's a customer-facing "pay by" line.
  due_by?: string
}

export interface Invoice {
  id: string
  invoice_number: string
  customer_id: string
  subscription_id: string
  status: string
  payment_status: string
  currency: string
  subtotal_cents: number
  discount_cents: number
  tax_amount_cents: number
  tax_rate: number
  tax_name: string
  tax_country?: string
  tax_id?: string
  tax_provider?: string
  tax_calculation_id?: string
  tax_transaction_id?: string
  tax_reverse_charge?: boolean
  tax_exempt_reason?: string
  // Tax-deferral state — populated when tax calculation hit a transient
  // upstream failure and is awaiting retry (or has exhausted retries).
  // 'ok' is the happy path; 'pending' / 'failed' surface in the operator
  // context card so debugging context is one click away.
  tax_status?: 'ok' | 'pending' | 'failed'
  tax_pending_reason?: string
  tax_retry_count?: number
  tax_deferred_at?: string
  // tax_error_code is the typed taxonomy of tax_pending_reason —
  // populated for invoices deferred after migration 0067. The
  // attention surface picks it up; raw consumers can read it directly.
  tax_error_code?: string
  // attention is the unified "needs operator attention" surface,
  // computed server-side from tax_status / tax_error_code /
  // payment_status / due_at. Omitted entirely when
  // the invoice is healthy. See ADR-009.
  attention?: InvoiceAttention
  // Present only when this written-off invoice CANNOT be charged, and says
  // why. Absent means recoverable (or not written off at all).
  recovery_block?: RecoveryBlock
  total_amount_cents: number
  amount_due_cents: number
  amount_paid_cents: number
  credits_applied_cents: number
  billing_period_start: string
  billing_period_end: string
  // Inclusive last-day period string ("Jun 1, 2028 – Jun 30, 2028"), authored
  // by the backend in the invoice's billing TZ (ADR-058 / ADR-074). Render
  // verbatim; do NOT recompute from the half-open start/end. Empty/omitted for
  // one-off invoices.
  billing_period_display?: string
  // The IANA TZ the invoice's period boundaries are anchored in — the org
  // billing timezone resolved and DENORMALIZED onto the invoice at issue
  // (ADR-077), so an issued invoice's dates stay fixed even if the org later
  // changes its timezone. Pass to formatCivil* for any per-line period the
  // backend doesn't author (the line-item "Covers …" note); empty for
  // ad-hoc/legacy invoices → helpers fall back to the live tenant TZ.
  billing_timezone?: string
  stripe_payment_intent_id?: string
  last_payment_error?: string
  issued_at?: string
  due_at?: string
  voided_at?: string
  paid_at?: string
  // public_token is populated at finalize. Drafts and pre-addendum
  // finalized invoices (migrated-in but never rotated) carry empty.
  public_token?: string
  created_at: string
  // is_simulated: this invoice's domain timestamps were stamped on a test
  // clock's simulated time (authoritative, set at write time). Drives the
  // "simulated" badge on header dates + the activity timeline. Live invoices
  // are always false.
  is_simulated?: boolean
}

export interface LineItem {
  id: string
  line_type: string
  description: string
  quantity: number
  unit_amount_cents: number
  // Full-precision per-unit price in decimal cents (e.g. "0.3" = $0.003).
  // Server-side display value (ADR-054): the stamped NOMINAL configured rate
  // on flat usage lines, falling back to amount_cents ÷ quantity only when no
  // nominal rate is stamped — so on flat lines it deliberately may differ from
  // amount ÷ quantity. Render with formatRate so sub-cent rates don't collapse
  // to "$0.00" like unit_amount_cents would.
  unit_amount_decimal?: string
  amount_cents: number
  total_amount_cents: number
  tax_amount_cents?: number
  tax_rate?: number
  tax_jurisdiction?: string
  tax_code?: string
  // tax_reason is the Stripe-canonical structured taxability_reason
  // (e.g. "standard_rated", "reverse_charge", "not_collecting",
  // "customer_exempt"). Empty for non-Stripe providers. The dashboard
  // surfaces a small badge for non-trivial values; see taxReasonLabel
  // in lib/taxReasons.ts for the rendering contract.
  tax_reason?: string
  currency: string
  pricing_mode?: string
  // ADR-031: explicit per-line period covered by the charge. Stamped
  // on base-fee lines so an invoice mixing in_advance base (next
  // period) with in_arrears usage (elapsed period) renders the
  // correct range per row. When null on a non-base line, the line
  // covers the invoice's billing_period_start/end.
  billing_period_start?: string | null
  billing_period_end?: string | null
}

export interface TimelineEvent {
  // Stable per-row key from the backend (outbox/attempt/dunning/CN row
  // ids; deterministic synthetics for lifecycle rows). Composite
  // timestamp keys collided once same-instant rows existed (ADR-104).
  id?: string
  timestamp: string
  // 'audit' is emitted by the subscription timeline (T0-18); invoice
  // timeline keeps 'stripe' and 'dunning'. Kept as a string union so the
  // renderer can default-case unknown sources rather than hiding them.
  source: 'stripe' | 'dunning' | 'audit' | string
  event_type: string
  status: string
  description: string
  error?: string
  amount_cents?: number
  currency?: string
  payment_intent_id?: string
  attempt_count?: number
  // Sub-line rendered beneath the row description. Used today by
  // invoice.paid for the payment instrument ("via Visa •••• 4242");
  // event types may add their own contextual detail later.
  // ADR-020.
  detail?: string
  // RFC3339 timestamp the detail prefix refers to (e.g. for the
  // "Auto-resumes" / "On" / "New trial end:" cases). When set, the
  // renderer formats it in tenant TZ via formatDateTime so the sub-
  // line stays consistent with the main row timestamp instead of
  // mixing the operator's TZ with backend-baked UTC.
  detail_timestamp?: string
  // Audit-sourced events carry the actor who performed the action. The
  // invoice payment timeline never populates these (webhook + dunning are
  // system-driven) so they're strictly optional.
  actor_type?: string
  actor_name?: string
  actor_id?: string
  // Backend-set authoritative flag. Semantics differ slightly by
  // surface, but always per-row (no client-side heuristic):
  // - Subscription activity timeline: true when the event's effect
  //   landed on a clock-pinned timeline. The row's `timestamp` is
  //   wall-clock (audit_log.created_at, per ADR-030 2026-05-28
  //   amendment); when this flag is true the simulated effect time
  //   is in sim_effective_at + test_clock_id below.
  // - Invoice payment timeline: true when the event's timestamp
  //   IS in simulated time (engine-driven on a clock-pinned sub).
  is_simulated?: boolean
  // RFC3339 simulated effect time. Populated alongside is_simulated
  // on operator-action audit rows for clock-pinned entities. Used
  // by the subscription activity timeline to render a subline
  // "Effect on test clock <id> at <sim time>" under the wall-clock
  // primary timestamp — mirrors the AuditLog page chip pattern.
  sim_effective_at?: string
  test_clock_id?: string
  // Invoice timeline (ADR-104): the REAL-WORLD instant of a row whose
  // primary `timestamp` is on the entity's simulated calendar — rendered
  // as a muted "Recorded <wall>" subline (operator contract, Invariant A:
  // any row whose two calendars differ shows both). Absent when the
  // calendars coincide.
  recorded_at?: string
}

export interface CreditBalance {
  customer_id: string
  balance_cents: number
  total_granted: number
  total_used: number
  total_expired: number
}

// Per-grant burndown row (commit/promo drawdown surfaces, 2026-07-06).
export interface CreditGrantSummary {
  id: string
  entry_type: string
  grant_kind?: 'commit' | 'promotional' | ''
  description: string
  amount_cents: number
  consumed_cents: number
  remaining_cents: number
  expires_at?: string
  source_invoice_id?: string
  created_at: string
}

export interface CreditGrantsResponse {
  grants: CreditGrantSummary[]
  commit_remaining_cents: number
  promotional_remaining_cents: number
  other_remaining_cents: number
}

// CustomerPaymentMethod mirrors the operator-facing JSON shape from
// GET /v1/customers/{id}/payment-methods. Card metadata is the
// PCI-safe subset (brand + last4 + exp); we never receive raw PAN.
export interface CustomerPaymentMethod {
  id: string
  type: string
  card_brand?: string
  card_last4?: string
  card_exp_month?: number
  card_exp_year?: number
  is_default: boolean
  created_at: string
}

export interface CreditLedgerEntry {
  id: string
  customer_id: string
  entry_type: string
  amount_cents: number
  balance_after: number
  description: string
  invoice_id: string
  expires_at?: string
  created_at: string
  // ADR-078: cost-basis class of a grant. 'commit' = prepaid commit funded
  // by an invoice; 'promotional' = free marketing credits (drained first);
  // absent = unclassified (paid class).
  grant_kind?: string
  // The invoice that funded a commit grant.
  source_invoice_id?: string
}

export interface CustomerOverview {
  customer_id: string
  active_subscriptions: Subscription[]
  recent_invoices: Invoice[]
  // Aggregate AR exposure — sum + count of unpaid finalized invoices
  // (pending / failed / unknown payment_status, excluding voided +
  // uncollectible). Industry-standard customer-page surface
  // (Stripe / Lago / Chargebee / Recurly) so operators see total
  // outstanding at a glance instead of summing per-invoice.
  outstanding_balance?: {
    total_cents: number
    unpaid_count: number
  }
}

export interface Meter {
  id: string
  key: string
  name: string
  unit: string
  aggregation: string
  rating_rule_version_id: string
  created_at: string
}

export type BillTiming = 'in_advance' | 'in_arrears'

export interface Plan {
  id: string
  code: string
  name: string
  currency: string
  billing_interval: string
  status: string
  base_amount_cents: number
  // ADR-031: when the recurring base bills. Defaults to 'in_arrears'
  // on existing plans. 'in_advance' opts the plan into first-invoice-
  // on-create + cancel-proration. Usage lines are always arrears.
  base_bill_timing: BillTiming
  meter_ids: string[]
  tax_code?: string
  created_at: string
}

// A negotiated per-customer price (ADR-070). rule_key is the identity —
// the deal follows the rule across version publishes; rating_rule_version_id
// only records which version the operator was looking at (provenance).
export interface PriceOverride {
  id: string
  rule_key: string
  customer_id: string
  rating_rule_version_id: string
  mode: string
  flat_amount_cents: string
  graduated_tiers?: { up_to: number; unit_amount_cents: string }[]
  package_size: number
  package_amount_cents: number
  reason?: string
  active: boolean
  created_at: string
}

export interface RatingRule {
  id: string
  rule_key: string
  name: string
  version: number
  mode: string
  currency: string
  // Per-unit rates are decimal cents serialized as strings (Stripe
  // unit_amount_decimal model) so sub-cent prices bill exactly. package_size /
  // package_amount_cents stay whole — a package is a fixed fee for a block.
  flat_amount_cents: string
  graduated_tiers?: { up_to: number; unit_amount_cents: string }[]
  package_size: number
  package_amount_cents: number
  // Per-unit rate charged for units beyond full packages (decimal-string
  // cents) — the billing engine charges it; omitting it from display
  // implies beyond-package usage is free.
  overage_unit_amount_cents: string
  lifecycle_state: string
  created_at: string
}

export interface UsageEvent {
  id: string
  customer_id: string
  meter_id: string
  subscription_id: string
  // String-encoded NUMERIC(38, 12) — decimal precision for fractional GPU-hours
  // and partial tokens. Coerce via Number() / parseFloat() at display time.
  quantity: string
  // Free-form dimensions per docs/design-multi-dim-meters.md, subset-matched
  // by pricing rules.
  dimensions?: Record<string, string | number | boolean>
  idempotency_key: string
  timestamp: string
}

// Response shape of GET /v1/usage-events/aggregate. Mirrors the same
// filter scope as listUsageEvents (customer_id, meter_id, from, to,
// dimensions) but ignores limit/offset — by_meter is the full filtered
// breakdown, not a page. total_units + by_meter[].total are decimal
// strings (ADR-005) for full NUMERIC(38, 12) round-trip; total_events
// and the active_* counts are integer JSON numbers. See
// internal/usage/store.go (Aggregate, MeterTotal).
export interface UsageEventsAggregate {
  total_events: number
  total_units: string
  active_meters: number
  active_customers: number
  by_meter: { meter_id: string; total: string }[]
}

export type MeterAggregationMode =
  | 'sum'
  | 'count'
  | 'last_during_period'
  | 'last_ever'
  | 'max'

export interface MeterPricingRule {
  id: string
  meter_id: string
  rating_rule_version_id: string
  dimension_match: Record<string, string | number | boolean>
  aggregation_mode: MeterAggregationMode
  priority: number
  created_at: string
}

// CustomerUsage — response of GET /v1/customers/{id}/usage. Live shape from
// internal/usage/customer_usage.go (PR #26). `totals` is unconditionally an
// array (single-currency tenants get length 1) so the client iterates without
// branching. `warnings` is a plain string list — the backend opted for human
// messages rather than structured codes since dashboards just render them.
export interface CustomerUsage {
  customer_id: string
  period: {
    from: string
    to: string
    source: 'current_billing_cycle' | 'explicit'
  }
  subscriptions: CustomerUsageSubscription[]
  meters: CustomerUsageMeter[]
  totals: { currency: string; amount_cents: number }[]
  warnings: string[]
  // Daily buckets for the chart. One entry per UTC day in [from, to)
  // with missing days zero-filled server-side. per_meter is keyed by
  // meter_id with the day's total quantity (decimal-string).
  buckets: CustomerUsageBucket[]
}

export interface CustomerUsageBucket {
  bucket_start: string
  per_meter: Record<string, string>
}

export interface CustomerUsageSubscription {
  id: string
  plan_id: string
  plan_name: string
  currency: string
  current_period_start: string
  current_period_end: string
}

export interface CustomerUsageMeter {
  meter_id: string
  meter_key: string
  meter_name: string
  unit: string
  currency: string
  total_quantity: string
  total_amount_cents: number
  rules: CustomerUsageRule[]
}

export interface CustomerUsageRule {
  rating_rule_version_id: string
  rule_key: string
  dimension_match?: Record<string, string | number | boolean>
  quantity: string
  amount_cents: number
  // Per-unit price in DECIMAL CENTS (nominal rate for flat rules, effective
  // amount÷qty for tiered) — the SAME value the invoice line shows. Render with
  // formatRate so sub-cent rates never collapse to $0.00. Absent on the
  // FE-synthesized "__other__" catch-all bucket.
  unit_amount_decimal?: string
  // True on the bucket of events that matched NO pricing rule (meter has no
  // default binding): billed nothing by the engine. Rendered as a warning row
  // — usually a mislabeled dimension value on a matrix-priced meter.
  unmatched?: boolean
}

export interface UsageSummary {
  customer_id: string
  meters: Record<string, number>
  total_events: number
}

export type CustomerTaxStatus = 'standard' | 'exempt' | 'reverse_charge'

export interface BillingProfile {
  customer_id: string
  legal_name: string
  // email removed in migration 0100 — customers.email is the single
  // canonical recipient. Dual-email override caused bounce-key skew.
  phone: string
  address_line1: string
  address_line2: string
  city: string
  state: string
  postal_code: string
  country: string
  currency: string
  tax_status: CustomerTaxStatus
  tax_exempt_reason: string
  tax_id: string
  tax_id_type: string
  profile_status: string
}

export interface TenantSettings {
  tenant_id: string
  default_currency: string
  timezone: string
  invoice_prefix: string
  net_payment_terms: number
  tax_provider: 'none' | 'manual' | 'stripe_tax'
  tax_rate: number
  tax_name: string
  tax_inclusive: boolean
  default_product_tax_code: string
  company_name: string
  company_address_line1: string
  company_address_line2: string
  company_city: string
  company_state: string
  company_postal_code: string
  company_country: string
  company_email: string
  company_phone: string
  logo_url: string
  brand_color: string
  tax_id: string
  support_url: string
  invoice_footer: string
  // ADR-078: arms the credit.balance_low webhook — a customer's balance
  // crossing below this many cents fires the event. null/absent = off.
  credit_balance_low_threshold_cents?: number | null
}

export interface StripeProviderCredentials {
  id: string
  tenant_id: string
  livemode: boolean
  stripe_account_id?: string
  stripe_account_name?: string
  secret_key_prefix?: string
  secret_key_last4: string
  publishable_key: string
  webhook_secret_last4?: string
  has_webhook_secret: boolean
  verified_at?: string | null
  last_verified_error?: string
  created_at: string
  updated_at: string
  // retries_queued is set ONLY on the POST /settings/stripe
  // (Connect) response — the count of invoices stuck on tax
  // provider-config errors that the server's post-connect
  // goroutine is about to retry in the background. Always 0 / unset
  // on List / Get responses. ADR-019.
  retries_queued?: number
}

// InvoicePreview is the response shape for both /v1/billing/preview/{id}
// (in-app debug route) and /v1/invoices/create_preview (Stripe Tier 1
// surface). Per-(meter, rule) lines mean multi-dim meters render one row
// per rule with dimension_match echoed; totals[] is always-array (one
// entry per distinct currency, even when there's only one) so a single
// reader handles both shapes. quantity is a decimal STRING per ADR-005
// — fractional AI-usage primitives (GPU-hours, cached-token ratios)
// round-trip without precision loss.
export interface InvoicePreview {
  customer_id: string
  subscription_id: string
  plan_name: string
  billing_period_start: string
  billing_period_end: string
  lines: InvoicePreviewLine[]
  totals: InvoicePreviewTotal[]
  warnings: string[]
  generated_at: string
}

export interface InvoicePreviewLine {
  line_type: string
  description: string
  meter_id?: string
  rating_rule_version_id?: string
  rule_key?: string
  dimension_match?: Record<string, unknown>
  currency: string
  quantity: string
  unit_amount_cents: number
  amount_cents: number
  pricing_mode?: string
}

export interface InvoicePreviewTotal {
  currency: string
  amount_cents: number
}

// DunningPolicy = a named campaign (ADR-036). Multi-policy-per-tenant;
// one row per (tenant, livemode) carries is_default=true and is used
// when a customer has no explicit dunning_policy_id assignment.
export interface DunningPolicy {
  id: string
  name: string
  enabled: boolean
  is_default: boolean
  retry_schedule: string[]
  max_retry_attempts: number
  // The two terminal decisions (ADR-112): 'none' | 'pause' | 'cancel' and
  // 'none' | 'mark_uncollectible'. They replaced a single `final_action`,
  // which could not express "cancel and write off".
  final_subscription_action: string
  final_invoice_action: string
  grace_period_days: number
  created_at?: string
  updated_at?: string
}

// DunningPolicyWithCount augments the policy list rows with the
// explicit-assignment count for the admin page's "N customers
// assigned" badge.
export interface DunningPolicyWithCount extends DunningPolicy {
  assigned_customers: number
}

export interface DunningRun {
  id: string
  invoice_id: string
  customer_id: string
  policy_id: string
  state: string
  reason: string
  attempt_count: number
  last_attempt_at?: string
  next_action_at?: string
  paused: boolean
  resolved_at?: string
  resolution: string
  created_at: string
  // Denormalized invoice fields (backend LEFT JOIN). Empty/zero when
  // the joined invoice can't be resolved (rare: deleted, RLS gap).
  invoice_number?: string
  invoice_amount_due_cents?: number
  invoice_currency?: string
  // Owning sub's test-clock frozen_time when applicable. Used as the
  // "now" baseline for relative-time rendering on this row (Next
  // Retry / Started). Missing for wall-clock runs — renderer falls
  // back to Date.now(). Authoritative; replaces prior heuristic.
  effective_now?: string
  test_clock_id?: string
}

export interface DunningEvent {
  id: string
  run_id: string
  event_type: string
  state: string
  reason: string
  attempt_count: number
  created_at: string
}

// Backend-computed aggregate counts across ALL dunning runs for the
// tenant; at_risk_cents sums only invoices in the tenant's default
// currency (runs in other currencies still count in the counts but are
// excluded from the sum). Source of truth for the dashboard stat cards;
// computing these client-side from a paginated /runs response
// undercounts as soon as runs exceed the page size.
export interface DunningStats {
  active_count: number
  escalated_count: number
  resolved_count: number
  at_risk_cents: number
}

export interface CreditNote {
  id: string
  invoice_id: string
  customer_id: string
  credit_note_number: string
  status: string
  reason: string
  subtotal_cents: number
  total_cents: number
  refund_amount_cents: number
  credit_amount_cents: number
  out_of_band_amount_cents: number
  tax_amount_cents?: number
  tax_transaction_id?: string
  refund_status: string
  currency: string
  issued_at?: string
  voided_at?: string
  created_at: string
  // Domain timestamps (issued_at/created_at) are simulated test-clock
  // time when true — same contract as Invoice.is_simulated.
  is_simulated?: boolean
}

export interface AuditEntry {
  id: string
  actor_type: string
  actor_id: string
  actor_name?: string
  action: string
  resource_type: string
  resource_id: string
  resource_label?: string
  metadata?: Record<string, unknown>
  ip_address?: string
  request_id?: string
  created_at: string
  // Sim axis (ADR-090 §5). Present only on rows emitted inside a test clock's
  // world. created_at stays wall-clock ("when the operator clicked");
  // sim_effective_at is the simulated instant the clock STOOD AT when the
  // mutation happened. An advance does not replay the intervening months — it
  // stands at the new frozen_time and settles what came due there — so every row
  // an advance produces shares one sim instant. There is deliberately no
  // "order by simulated time": it would order nothing.
  sim_effective_at?: string
  test_clock_id?: string
  // The subject is gone. Set when the row's test clock was torn down (ADR-086
  // hard-deletes the whole simulated graph). The row survives on purpose — it is
  // the only record left that the simulation happened — but the invoice or
  // customer it names does not, so a "View" link on it 404s.
  subject_deleted?: boolean
}

export interface AuditSimClock {
  id: string
  name: string
}

export interface AuditFilterOptions {
  actions: string[]
  resource_types: string[]
  // Clocks the LOG has seen — including ones already torn down (ADR-086
  // hard-deletes the clock and its whole graph), which is exactly when the
  // audit log becomes the simulation's only record. Sourced from audit rows,
  // not from test_clocks, so the picker does not go empty at that moment.
  test_clocks?: AuditSimClock[]
}

export interface WebhookEndpoint {
  id: string
  url: string
  description: string
  events: string[]
  active: boolean
  created_at: string
  secret_last4?: string
  // Populated only during a rotation's 72h grace window. When set, the
  // dispatcher signs outbound events with BOTH the current and previous
  // secrets so the receiver's verifier can be staged without an outage.
  secondary_secret_last4?: string
  secondary_secret_expires_at?: string
}

export interface WebhookEvent {
  id: string
  event_type: string
  payload: Record<string, unknown>
  created_at: string
  // Week 6: replay-tree pivot. Original events leave this nil; replay
  // clones point at the root original (single-pivot rule — the chain is
  // collapsed at create time so it's always one hop). Optional in the TS
  // type because legacy events written before migration 0058 don't carry
  // it.
  replay_of_event_id?: string | null
}

// SSE-shaped projection of a webhook event the live tail consumes via
// EventSource on /v1/webhook_events/stream. Snake_case keys pinned by
// TestWireShape_WebhookEventsStream_SnakeCase.
export interface WebhookEventStreamFrame {
  event_id: string
  event_type: string
  customer_id: string
  status: string  // "pending" | "succeeded" | "failed"
  last_attempt_at: string | null
  created_at: string
  livemode: boolean
  replay_of_event_id: string | null
}

// Per-attempt timeline row served by GET /v1/webhook_events/{id}/deliveries.
// Each row carries everything the dashboard's expandable timeline shows
// plus the request_payload_sha256 the diff viewer uses to flag "payload
// identical between attempts" (Stripe-style replay's common case). Keys
// pinned by TestWireShape_WebhookEventDeliveries.
export interface WebhookDeliveryView {
  id: string
  event_id: string
  endpoint_id: string
  attempt_no: number
  status: string  // "pending" | "succeeded" | "failed"
  status_code: number
  response_body: string
  error: string
  request_payload_sha256: string
  attempted_at: string
  completed_at: string | null
  next_retry_at: string | null
  is_replay: boolean
  replay_event_id: string
}

export interface WebhookDeliveriesResponse {
  root_event_id: string
  deliveries: WebhookDeliveryView[]
}

export interface ApiKeyInfo {
  id: string
  key_prefix: string
  key_type: string
  name: string
  created_at: string
  expires_at?: string
  revoked_at?: string
  last_used_at?: string
}

// Matches OverviewResponse in internal/analytics/overview.go. All money is
// in cents; rates are in [0, 1].
export interface AnalyticsOverview {
  period: string
  mrr: number
  mrr_prev: number
  arr: number
  arr_prev: number
  revenue: number
  revenue_prev: number
  outstanding_ar: number
  avg_invoice_value: number
  credit_balance_total: number
  active_customers: number
  new_customers: number
  active_subscriptions: number
  trialing_subscriptions: number
  paid_invoices: number
  failed_payments: number
  open_invoices: number
  dunning_active: number
  refunds_needing_attention: number
  usage_events: number
  logo_churn_rate: number
  revenue_churn_rate: number
  nrr: number
  dunning_recovery_rate: number
  mrr_movement: MRRMovementTotals
}

export interface MRRMovementTotals {
  new: number
  expansion: number
  contraction: number
  churned: number
  net: number
}

export interface MRRMovementPoint {
  date: string
  new: number
  expansion: number
  contraction: number
  churned: number
  net: number
  [k: string]: unknown
}

export interface MRRMovementResponse {
  period: string
  data: MRRMovementPoint[]
  totals: MRRMovementTotals
}

export interface RevenueDataPoint {
  date: string
  revenue_cents: number
  invoice_count: number
  [k: string]: unknown
}

export interface UsagePoint {
  date: string
  events: number
  quantity: number
  [k: string]: unknown
}

export interface TopMeterUsage {
  meter_id: string
  meter_name: string
  key: string
  events: number
  quantity: number
}

export interface UsageAnalyticsResponse {
  period: string
  data: UsagePoint[]
  top_meters: TopMeterUsage[]
  totals: { events: number; quantity: number }
}

export async function downloadPDF(invoiceId: string, invoiceNumber: string) {
  const res = await fetchWithTimeout(`${API_BASE}/invoices/${invoiceId}/pdf`, {
    credentials: 'include',
  })
  const blob = await res.blob()
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `${invoiceNumber}.pdf`
  a.click()
  URL.revokeObjectURL(url)
}

export async function downloadCreditNotePDF(creditNoteId: string, creditNoteNumber: string) {
  const res = await fetchWithTimeout(`${API_BASE}/credit-notes/${creditNoteId}/pdf`, {
    credentials: 'include',
  })
  if (!res.ok) {
    throw new Error(`Failed to download credit note PDF (${res.status})`)
  }
  const blob = await res.blob()
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `${creditNoteNumber || creditNoteId}.pdf`
  a.click()
  URL.revokeObjectURL(url)
}

const CURRENCY_SYMBOLS: Record<string, string> = {
  USD: '$', EUR: '€', GBP: '£', JPY: '¥', CNY: '¥',
  INR: '₹', BRL: 'R$', CAD: 'CA$', AUD: 'A$', CHF: 'CHF ',
  SGD: 'S$', HKD: 'HK$', NZD: 'NZ$', SEK: 'kr ', NOK: 'kr ',
  DKK: 'kr ', ZAR: 'R', KRW: '₩', MXN: 'MX$', PLN: 'zł',
  CZK: 'Kč ', HUF: 'Ft ', ILS: '₪', AED: 'AED ', SAR: 'SAR ',
  THB: '฿', MYR: 'RM ', IDR: 'Rp ', PHP: '₱', VND: '₫',
  CLP: 'CL$', COP: 'CO$', PEN: 'S/', ARS: 'AR$', TWD: 'NT$',
}

let _activeCurrency = 'USD'

export function setActiveCurrency(code: string) {
  _activeCurrency = code.toUpperCase()
}

export function getActiveCurrency(): string {
  return _activeCurrency
}

export function getCurrencySymbol(code?: string): string {
  return CURRENCY_SYMBOLS[(code || _activeCurrency).toUpperCase()] || (code || _activeCurrency) + ' '
}

export function formatCents(cents: number, currency?: string): string {
  const sign = cents < 0 ? '-' : ''
  const abs = Math.abs(cents)
  const symbol = getCurrencySymbol(currency)
  return `${sign}${symbol}${Math.floor(abs / 100).toLocaleString('en-US')}.${String(abs % 100).padStart(2, '0')}`
}

// formatTaxRate renders a tax-rate percent (without the % sign) at up to 4
// decimal places, trailing zeros trimmed: 8.875 → "8.875", 18 → "18",
// 9.975 → "9.975". The tax rate is a NUMERIC(7,4) statutory rate (ADR-042/043);
// toFixed(2) silently truncates real 4-decimal rates (8.875 → "8.88"), which
// misstates the rate on customer-facing invoices. Callers append "%".
export function formatTaxRate(rate: number): string {
  return rate.toFixed(4).replace(/\.?0+$/, '')
}

// formatRate renders a per-unit price that is carried as decimal cents
// (e.g. "0.0003" cents = $0.000003 per unit, the Stripe unit_amount_decimal
// model). The rate arrives as a string from the API to preserve precision —
// formatCents must NOT be used here because it floors to whole cents and would
// collapse any sub-cent rate to $0.00. We divide by 100 via STRING decimal
// shift (not parseFloat/toFixed, which round real rates like $0.000000075 =
// gpt-4o-mini cache_read and can collapse tiny rates to $0.00) so every digit
// of the rate survives; we keep a minimum of 2 fractional digits and never
// round a nonzero rate down to "$0.00". Invoice line amounts and totals stay
// whole cents (formatCents).
// dollarsToRateCents converts an operator-entered dollar amount into decimal
// cents as a string, preserving sub-cent precision (e.g. "0.000003" dollars →
// "0.0003" cents). Per-unit rates can be fractions of a cent, so we must not
// round to whole cents. We shift the decimal point two places right via string
// math rather than multiplying by 100 in floating point, which would introduce
// artifacts like 0.000003 * 100 = 0.00030000000000000003.
export function dollarsToRateCents(input: string): string {
  const raw = (input || '0').trim()
  const m = /^(-?)(\d*)(?:\.(\d*))?$/.exec(raw)
  if (!m) return '0'
  const sign = m[1]
  let intPart = m[2] || '0'
  let frac = m[3] || ''
  // Shift two places right: borrow up to two digits from the fraction.
  intPart += frac.slice(0, 2).padEnd(2, '0')
  frac = frac.slice(2)
  intPart = intPart.replace(/^0+(?=\d)/, '')
  let out = frac ? `${intPart}.${frac}` : intPart
  out = out.replace(/(\.\d*?)0+$/, '$1').replace(/\.$/, '')
  return out === '0' || out === '' ? '0' : `${sign}${out}`
}

export function formatRate(cents: string | number, currency?: string): string {
  const symbol = getCurrencySymbol(currency)
  let s = (typeof cents === 'number' ? String(cents) : cents).trim()
  const neg = s.startsWith('-')
  if (neg) s = s.slice(1)
  if (!/^\d*\.?\d*$/.test(s) || s === '' || s === '.') return `${symbol}0.00`
  const [intRaw = '', fracRaw = ''] = s.split('.')
  // value = (intRaw + fracRaw) with the point after intRaw.length digits;
  // dividing by 100 moves the point two places left.
  const digits = (intRaw || '') + fracRaw
  const pointPos = (intRaw.length || 0) - 2
  let whole: string
  let frac: string
  if (pointPos <= 0) {
    whole = '0'
    frac = '0'.repeat(-pointPos) + digits
  } else {
    whole = digits.slice(0, pointPos)
    frac = digits.slice(pointPos)
  }
  whole = whole.replace(/^0+(?=\d)/, '') || '0'
  frac = frac.replace(/0+$/, '')
  if (frac.length < 2) frac = frac.padEnd(2, '0')
  const sign = neg && !(whole === '0' && /^0*$/.test(frac)) ? '-' : ''
  // Group the integer part with thousands separators (string-based, so very
  // large integer parts keep full precision) to match formatCents and the Go
  // PDF formatRate — otherwise a high unit price renders "$12345.00" next to a
  // comma-grouped Amount column.
  const groupedWhole = whole.replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  return `${sign}${symbol}${groupedWhole}.${frac}`
}

// _tenantTimezone is module-scoped state seeded once at app boot
// (after /v1/settings resolves) via setTenantTimezone. formatDate /
// formatDateTime read from it as their default so every existing
// display call site automatically renders in tenant TZ — no per-
// callsite churn. Not reactive: tenant TZ rarely changes during a
// session, and the Settings save flow is followed by a navigation
// that re-bootstraps. Acceptable for v1; revisit if multi-tenant-
// switcher UX lands. See ADR-010.
let _tenantTimezone: string | null = null

export function setTenantTimezone(tz: string | null): void {
  _tenantTimezone = tz || null
}

export function getTenantTimezone(): string | null {
  return _tenantTimezone
}

// formatDate / formatDateTime — default to tenant TZ when set,
// browser-local otherwise. Pass an explicit `timezone` to override
// (used by ApiKeys to be defensive even before the bootstrap-time
// setTenantTimezone call resolves).
//
// formatDate stays bare (no zone label) — date-only displays are
// visually noisy with a TZ stamp and the date is unambiguous at
// day resolution for normal use.
//
// formatDateTime appends a zone abbreviation when rendering in
// tenant TZ ("May 5, 2026, 2:14 PM PDT") so operators have an
// explicit cue about the rendering frame. Browser-local fallback
// stays unlabelled (matches existing behaviour to avoid surprising
// pre-bootstrap renders).
export function formatDate(iso: string, timezone?: string): string {
  const tz = timezone || _tenantTimezone || undefined
  return new Date(iso).toLocaleDateString('en-US', {
    year: 'numeric', month: 'short', day: 'numeric',
    ...(tz ? { timeZone: tz } : {}),
  })
}

export function formatDateTime(iso: string, timezone?: string): string {
  const tz = timezone || _tenantTimezone || undefined
  return new Date(iso).toLocaleString('en-US', {
    year: 'numeric', month: 'short', day: 'numeric',
    hour: 'numeric', minute: '2-digit',
    ...(tz ? { timeZone: tz, timeZoneName: 'short' } : {}),
  })
}

// Relative-time formatting moved to lib/effectiveNow (timeAgo) — it requires a
// branded EffectiveNow anchor so a clock-pinned surface can't silently fall back
// to wall-clock. See lib/effectiveNow.ts.
