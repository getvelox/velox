# ADR-111: A write-off has no tax leg

**Status:** Accepted (2026-08-05)
**Relates to:** [ADR-110](110-written-off-invoices-close-self-service-payment.md)
(written-off invoices close self-service payment), [ADR-107](107-unknown-is-terminal-until-a-human.md)
(parked invoices), ADR-108 (parked search-and-adopt)

## Context

`MarkUncollectible` reverses the invoice's tax with the provider. Void and
credit notes do too. This ADR says the write-off should not, and records why
the other two still should.

It was written after a design that got most of its own reasoning wrong. Three
of four proposed principles were abandoned or refuted on inspection; what
survives is one rule and a set of corrections. The refutations are kept below
because each is a claim someone will otherwise make again.

## Decision

**A write-off has no tax leg. `MarkUncollectible` does not reverse tax.**

A write-off changes *who paid*, not *what was sold*. The service was delivered,
the invoice was correct, the tax was correctly reported on accrual. The entry
is Dr bad debt / Cr AR — no tax line.

Void and credit note keep reversing, and that asymmetry is the whole point:

| Event | What changed | Tax |
|---|---|---|
| Void | the supply is annulled | **reverse** |
| Credit note | what was supplied changed | **reverse** |
| Write-off | only collection failed | **nothing** |

### Why not the alternative

The coherent alternative is Stripe's: reverse on write-off **and** re-report on
recovery. Stripe does exactly this — `mark_uncollectible` decreases reported
tax, and *"Transitioning the state of an invoice from `uncollectible` to `paid`
through the Pay Invoices API"* increases it again
(https://docs.stripe.com/tax/reports, verified verbatim).

**Velox shipped the first half and then blocked the second half with a 409.**
That is the actual defect the tax gate was papering over.

The round trip is buildable — a re-report is a fresh calculation and a fresh
transaction, effective the date of the call, which is what the authorities
require anyway. It is rejected because it is more code, is legally premature in
at least one supported jurisdiction (below), and buys nothing at a tenant base
of zero.

## The decisive argument is in our own tree, not in tax law

Independent of any jurisdiction, this path is live today and has no human in
it:

1. an ambiguous charge parks an invoice (ADR-107)
2. write-off is the **only** legal exit, and the product's own attention banner
   advises it
3. the reversal fires — tax un-reported
4. the ADR-108 provider search keeps written-off parked invoices in scope
   (`internal/invoice/postgres.go`, `status IN ('finalized','uncollectible')`,
   shipped 2026-08-05 in #735)
5. the search finds the charge succeeded; `markPaidReportingTransition` admits
   `uncollectible → paid` unconditionally (`internal/invoice/postgres.go:1018`)
6. the invoice settles **paid, with its tax reversed, and nothing re-reports it**

No gate stands on that path — the tax gate guards the *card* route, not this
one. #735 was a correct fix for a real scan-exclusion sink, and it made this
reachable. Removing the reversal closes it by construction rather than by
adding a third guard.

Note the codebase had already found the same failure mode and guarded only its
narrow case: `MarkUncollectible`'s in-flight guard exists because *"reverse tax
+ flip to uncollectible now and the charge then settles, the tax for a real
collected sale has been reversed → the tenant under-remits"*
(`internal/invoice/service.go:1305-1308`). That guard covers a payment in
flight **at write-off time**. It does not cover the payment that arrives later,
which is the same failure with a longer gap.

## The jurisdictional argument (corroborating, not load-bearing)

Recovering tax remitted on an uncollected debt is **bad-debt relief** — a
separate claim, conditioned on facts Velox does not hold:

- **UK** — HMRC VAT Notice 700/18 §2.2: the VAT must already have been
  *"accounted for and paid to HMRC"*; the debt must be *"written off in your
  day to day VAT accounts and transferred to a separate bad debt account"*; and
  it must have *"remained unpaid for a period of 6 months"*.
  https://www.gov.uk/guidance/relief-from-vat-on-bad-debts-notice-70018
  A reversal fired at write-off time is therefore **premature by construction
  in the UK for every invoice**, not merely optional.
- **California** (CDTFA Reg. 1642), **New York** (20 NYCRR 534.7), **Texas**
  (34 TAC §3.302) all condition relief on the debt being charged off for
  federal income tax purposes under IRC §166 — an accounting event on the
  tenant's calendar, not ours.
- **Recovery after relief** requires re-reporting: HMRC 700/18 §3.14 — *"you
  must repay to us the VAT element included in the payment"*; CDTFA 1642 — the
  amount goes in *"the first return filed after receipt"*.

Deliberately corroborating rather than load-bearing: it is contested, and the
vendors split. **Anrok** directs users to *void* an invoice they do not want to
owe tax on, i.e. its sync keys on void and credit notes rather than on
`uncollectible` (help-center.anrok.com — direct fetch 403, **UNVERIFIED
verbatim**). Stripe reverses. Two serious vendors disagree, which is itself the
finding: this is a policy choice the platform makes on the tenant's behalf, and
it is not settled practice.

## Two false citations, deleted

The current behaviour was justified by two things that are not true:

- `internal/invoice/service.go:1050` attributed to Stripe: *"When an invoice is
  voided or marked uncollectible, you must reverse the corresponding tax
  transaction."* **That string does not exist in Stripe's documentation.**
  Stripe documents its own reporting behaviour and imposes no obligation on an
  external integrator.
- `internal/invoice/service.go:1049` cited **EU VAT Directive Art. 90** as
  authority for reversing automatically. Art. 90 says the opposite: reduction
  on non-payment happens *"under conditions which shall be determined by the
  Member States"*, and Art. 90(2) lets member states derogate entirely for
  non-payment.

Both are removed. Prose born false, load-bearing for a money decision.

## What this does NOT change, and the claims that were refuted

**The subscription still keeps billing after a write-off.** A proposal to pause
it was abandoned: the parity claim behind it was backwards. Stripe — *"Marking
an invoice as uncollectible results in the following: Stripe treats the
subscription as if the user paid and stops attempting to collect payment. **The
user's subscription continues as normal.**"* The pairing runs the other way at
Stripe: a cancel-on-retry-exhaustion setting is what marks the invoice
uncollectible. Velox's current behaviour is already parity. Pausing would also
have deleted a choice the dialog explicitly offers, and would have re-stranded
parked invoices on terminal subscriptions, where `SetPauseCollection` refuses
(`internal/subscription/postgres.go:625`) and write-off is the only exit.

**The tax gate is not dissolved by this ADR** — a claim made and refuted during
design. `RecoveryBlocksCharge` keys on `tax_transaction_id != ''`
(`internal/domain/invoice_payment_gate.go`), and `MarkTaxReversed` never clears
that column. Not reversing changes the gate's behaviour on **zero rows**; it
only falsifies the gate's stated reason. Re-keying it to `tax_reversed_at` is a
separate, explicit edit carried by the same PR — verified necessary: 2 live rows
would still fire it.

**The rule for block-vs-warn is the one already written in the file**, not a new
one: *refuse to CREATE a bad money event; never refuse to RECORD one that
already happened.* It splits by who moved the money, which is decidable. A
proposed alternative — split by who is harmed — was refuted because both
remaining gates harm both parties.

**"No machine charges a written-off debt" already holds** and needs no code:
every machine claim pins `status = 'finalized'`. It must NOT be restated as "no
machine writes to a written-off invoice" — the ADR-108 search and the payment
webhook settle written-off invoices deliberately, and that is recording a
payment that already happened, not initiating one.

## Consequences

- Under-remittance on recovery is impossible: nothing was ever un-reported.
- The tenant **over-remits** until they claim relief themselves. That is the
  safe direction — over-remitting is the tenant's money temporarily,
  under-remitting is a liability — and the claim is jurisdiction-conditioned,
  so it belongs to them.
- The relief hatch, minimum version: the invoice CSV export gains `tax_provider`
  and `tax_transaction_id`. It already carries `status`, `tax_amount_cents`,
  `uncollectible_at`, `due_at`, `paid_at` and `amount_paid_cents` — every input
  the UK/CA/NY/TX tests need. What it lacked was whether Velox had already
  reversed at the provider, which is the one fact that decides whether the
  tenant's own claim would be a double claim.
- Invoices written off **before** this change keep their reversal. Nothing can
  un-reverse them, and nothing breaks: the gate keys on a column this ADR does
  not touch.

## Deferred, with triggers

- **An operator-initiated "claim bad-debt relief" action** that fires the
  reversal when the tenant is actually entitled. Trigger: the first
  tax-registered tenant on accrual basis in a jurisdiction granting relief.
- **Re-report on recovery** (Stripe's model). Only needed if the above ships,
  since relief-then-recovery is what creates the repayment obligation.

## Separate defect found while measuring, NOT fixed here

`ListPendingTaxReversal` is bounded by `updated_at > now() - interval '24
hours'`. A reversal that fails and is not retried inside that window is
**stranded permanently**. Measured on the live database: **4 voided invoices
carrying $46.69 of tax** have a committed transaction, no `tax_reversed_at`,
and are past the window. For *voided* invoices the reversal is unambiguously
correct, so that is a live over-remittance with no recovery path. This ADR
narrows the sweep to voided-only, which does not touch the window; the window
is its own defect and needs its own fix.
