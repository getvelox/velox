# ADR-113: Nothing charges a written-off invoice

**Status:** Accepted (2026-08-06)
**Supersedes:** the 2026-08-05 amendment of
[ADR-110](110-written-off-invoices-close-self-service-payment.md) ("the
limitation is closed, by charging in place") — charge-in-place recovery is
removed. ADR-110's core decision (public pages closed) is untouched.
**Relates to:** [ADR-111](111-a-write-off-has-no-tax-leg.md),
[ADR-112](112-dunning-exhaustion-settles-two-questions.md)

## Decision

**A written-off invoice is settled by RECORDING writers only** — an offline
payment the operator types in, or the ADR-108 provider search adopting a
charge that turns out to have succeeded. **Nothing initiates money against
it.** `ClaimChargeForManualCollect` is back to `finalized` only; the three
recovery CAS gates, `RecoveryBlocksCharge`, the `recovery_block` API field,
and the "Charge customer" recovery UI are deleted.

Collection intent lives where ADR-112 put it: a tenant who still wants the
money **leaves the invoice open**. A write-off is the give-up assertion.

For a written-off debt whose customer returns, the documented practice is a
**fresh recovery invoice** on completely normal rails (hosted Pay page,
auto-charge once a card is on file, receipts, optional dunning). The old
invoice stays honestly written off; bad-debt recovery books as new income,
which is the standard accounting shape.

## Industry evidence (verified 2026-08-06)

The question asked of each platform: *once a debt is written off, what path
exists for collecting it later?*

| Platform | Write-off is… | Collect-later path | Charge the written-off object? |
|---|---|---|---|
| **Stripe** | a status flip, amount preserved | `POST /v1/invoices/{id}/pay` — charges the default or a specified PM, *"off_session… Defaults to true"* (api/invoices/pay, fetched verbatim); *"An uncollectible invoice might still be paid"* (ADR-110) | **YES — the only one** |
| **Chargebee** | an **adjustment credit note**; invoice reads "Paid" | remove the CN → *"the amount is reversed and recorded under the bad debt reversal column of the account summary report"* (KB: how-to-void-a-write-off-invoice) → due again → normal rails | no — reverse the artifact first |
| **Recurly** | Stop Collection → Failed + write-off credit invoice | *"Once an invoice is failed (via Stop Collection), it **cannot be reopened**."* (docs: invoice-management) | no — irreversible |
| **Zuora** *(search-snippet grade; docs JS-walled)* | a credit memo applied to the invoice | "unapply, cancel, or unpost the generated credit memo" → then collect | no — reverse the memo first |
| **Lago / Orb** | state doesn't exist (ADR-110: zero hits across both doc sets) | invoice simply stays open and payable | n/a — this IS "leave it open" |

**Charge-in-place: 1 of 6.** Chargebee and Zuora can reverse cleanly because
their write-off IS an artifact whose removal restores the receivable with an
audit trail. Stripe models write-off as a bare status and compensates by
making the status payable — affordable for Stripe because it owns its tax
ledger and re-reports on recovery. Velox had imported Stripe's bare-status
model *and* the payable-status compensation **without** Stripe's tax
machinery — the mismatch ADR-111 spent an arc cleaning up.

## The design argument (the evidence confirmed it)

- **The governing rule** distilled by this arc — *refuse to CREATE a bad
  money event; never refuse to RECORD one that already happened* — was
  violated by exactly one writer on the uncollectible state: the operator
  card charge. Every other writer records.
- **The three CAS gates were the symptom.** Tax-reversed, threshold-re-billed,
  relief-unapplied — each exists only because charge-in-place aims money at
  an object carrying stale state. A fresh recovery invoice has none of those
  hazards: tax computes fresh, the line is deliberate, the amount is typed.
- **ADR-112 dissolved the customer.** "Wrote it off but still want to
  collect" now has a first-class answer: don't write it off
  (`final_invoice_action: none`). The only remaining client was a
  mind-change after a formal give-up.
- **Risk sat on the wrong side**: off-session MIT on a months-old stored
  card for an abandoned debt is chargeback bait.

## What survives, deliberately

- **`uncollectible → paid` edge** — recording writers need it (offline
  payment, ADR-108 adoption). `markPaidReportingTransition` unchanged.
- **`RecoveryWarnsOnOfflinePayment`** and the `recovery_warning` API field —
  recording is never refused, but the operator is told when it will not
  reconcile (tax already reported uncollected / threshold re-billed / relief
  unapplied). The OpenAPI schema is renamed `RecoveryWarning`.
- **The setup-link on-ramp** (walk-d6, merged as #747) — card capture is
  account-scoped (ADR-110's own line); the card serves the recovery invoice
  and future billing. Its email already promises a person, not a machine.
- **Dunning-no-restart guards, timeline truth, CSV relief columns** — all
  independent of charge-in-place.

## Refuted / rejected on the way

- **Reopen (`uncollectible → finalized`)** — still rejected, same grounds as
  ADR-110's amendment: analytics recompute from current status, so a flip
  erases the write-off from history; and it silently re-arms every machine.
  Chargebee/Zuora reinstate honestly only because a reversal ARTIFACT
  remains; Velox's bare status has none. The fresh invoice gets the same
  outcome and leaves both objects truthful.
- **Keeping charge-in-place frozen behind a kill trigger** — proposed, then
  overtaken: the user asked for the design-truth answer with build/removal
  costs excluded, and on that question the evidence is one-sided.

## Consequences

- The operator collect endpoint answers `finalized`-only again; a
  written-off invoice gets a message naming the two real paths (recovery
  invoice / offline record) instead of a charge flow with three 409s behind
  it.
- `recovery_block` is gone from the API (pre-launch; the field was 1 day
  old). `recovery_warning` keeps its wire shape.
- FLOW D6 is rewritten around the surviving paths; the walked charge-in-place
  boxes are deleted, not left as history — a flow that can't be run is rot.
- Re-adding charge-in-place requires re-litigating this ADR's table, not
  just reverting a commit — `TestManualCollectClaim_RefusesWrittenOff` and
  `TestManualCollectClaim_RefusalIsStatusNotGates` pin the refusal in real
  Postgres and are mutation-verified against exactly that revert.
