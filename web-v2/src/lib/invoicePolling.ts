// Refetch cadence for the invoice detail page.
//
// Lives in its own module rather than in api.ts so it can be TESTED: api.ts
// pulls in the whole client dependency graph, which the node test runner
// cannot resolve, so anything defined there is effectively unpinnable. The
// rule this encodes was silently wrong for the entire bad-debt-recovery arc
// (a written-off invoice with a live charge stopped refreshing, freezing the
// page at the exact moment money was moving) and nothing could have caught it.
// Same reasoning as invoiceTerminal.ts.

// Self-contained on purpose: the node test runner cannot resolve this
// package's aliased/extensionless imports, so a module that imports anything
// relative is a module that cannot be tested. Both dependencies are tiny —
// the invoice shape is structural, and the in-flight rule is one comparison —
// and the rule is mirrored from status.ts's paymentIsUnresolved, which is
// pinned by its own callers in the pages.
type PollableInvoice = {
  status: string
  payment_status: string
  paid_at?: string | null
  tax_status?: string
}

function paymentIsUnresolved(paymentStatus: string): boolean {
  return paymentStatus === 'processing' || paymentStatus === 'unknown'
}

// pollIntervalForInvoice picks a refetch cadence based on the invoice's
// transient state. Detail pages plug this into useQuery({ refetchInterval })
// so the operator sees webhook-driven updates (Stripe payment.succeeded,
// tax retry resolution, dunning resolution) without manually refreshing.
//
// Cadences are tuned to the speed of the underlying signal:
//   - 2s  for in-flight charges (processing/unknown) — Stripe webhook
//          typically lands within 1-3s
//   - 5s  for tax-retry / payment-failed / dunning-active — backend
//          retries operate on second-to-minute scales
//   - 10s for awaiting-first-charge / setup-pending — slower changes,
//          gentler load
//   - false for terminal states (paid/voided/draft) — refetchOnWindowFocus
//          handles the rare "tab was open all day" case without polling
//
// Pre-launch / pre-SSE: polling is the right primitive here. Stripe
// Dashboard does the same. Upgrade to `/v1/webhook_events/stream` SSE
// when an operator complains about latency, not before.
export function pollIntervalForInvoice(invoice?: PollableInvoice): number | false {
  if (!invoice) return false
  // Drafts + voided are terminal-no-trailing-events.
  //
  // uncollectible is terminal ONLY while nothing is charging it. A bad-debt
  // recovery leaves the invoice written off with a live PaymentIntent, and
  // treating that as terminal froze the page at the exact moment money was
  // moving: the operator clicked Charge customer, one refetch drew the
  // in-flight banner, and nothing updated again until a manual reload — no
  // settle, no paid row, no status flip. The banner that exists to stop a
  // second operator charging again could never clear on its own.
  //
  // paymentIsUnresolved is the shared predicate, never re-typed here.
  if (invoice.status === 'draft' || invoice.status === 'voided') return false
  if (invoice.status === 'uncollectible' && !paymentIsUnresolved(invoice.payment_status)) return false
  // Just-paid invoices keep polling slowly for ~30s to catch trailing
  // events: receipt email lands 1-5s after MarkPaid (outbox dispatcher
  // drains async), dunning resolution fires after MarkPaid for
  // recovered-via-retry invoices, card-detail stamping (ADR-020) is a
  // second write after MarkPaid. Cutting polling the instant
  // payment_status flips to 'paid' makes the activity log appear
  // missing those rows. Same trailing-poll pattern Stripe Dashboard
  // and Recurly use after a status transition.
  if (invoice.payment_status === 'succeeded' || invoice.payment_status === 'paid') {
    if (invoice.paid_at) {
      const paidAtMs = Date.parse(invoice.paid_at)
      // eslint-disable-next-line no-restricted-syntax -- poll-freshness window is real elapsed wall-clock, not simulated time
      if (!isNaN(paidAtMs) && Date.now() - paidAtMs < 30_000) return 5000
    }
    return false
  }
  // In-flight charge — webhook resolution imminent.
  if (invoice.payment_status === 'processing' || invoice.payment_status === 'unknown') return 2000
  // Tax retry, dunning active, or post-decline waiting for retry.
  if (invoice.tax_status === 'pending' || invoice.tax_status === 'failed') return 5000
  if (invoice.payment_status === 'failed') return 5000
  // Awaiting first charge / customer setup.
  if (invoice.status === 'finalized' && invoice.payment_status === 'pending') return 10000
  return false
}
