# ADR-048: Credit clawbacks reverse proportional tax via the credit-note primitive

- Status: Accepted
- Date: 2026-06-06
- Supersedes/relates: ADR-039 (credits = the discount primitive), ADR-042 (integer proration math), the cancel/prebill-relief work (#22)

## Summary

A clawback credits a customer for unused time on a paid, taxed `in_advance` charge after a downgrade, a mid-cycle cancel, or a plan swap. Velox issues each clawback as a credit note (`creditnote.Service.CreateAndIssueAdjustment`), not as a bare net grant to the credit ledger. The credit note adds the gross amount the customer paid to their balance and reverses the matching output tax against the invoice that was paid. A net-only grant left the customer short by the tax, and the tenant over-reported tax. Amended 2026-06-15: a period can be funded by several invoices (the base invoice plus any mid-period upgrade invoice), so clawbacks find the full set with `invoice.FindFundingInvoicesForPeriod` and issue one credit note per invoice, each reversing tax against its own invoice. Cancel and swap split one authoritative unused total across those invoices rather than recomputing it per invoice. A plan downgrade takes the credit from the newest invoice first, and quantity or item changes split it proportionally, each piece capped at what that invoice can still be credited. Voiding or marking an invoice uncollectible reverses only the tax that its credit notes have not already reversed, because only credit notes reverse tax.

## Context

When Velox claws back part of an **already-paid, taxed `in_advance`** charge — a mid-cycle **plan downgrade**, a **mid-cycle cancel** of a paid prebill, or a **plan swap** — it credits the customer for the unused time. Today all three paths grant a **bare net (tax-exclusive) amount** to the customer credit ledger and reverse **no** output tax:

- downgrade proration — `subscription/handler.go` (`proratedCents < 0` → `GrantProration`)
- cancel paid-prebill — `engine.go BillOnCancel` (paid path → `creditGranter.Grant`)
- plan swap — `engine.go BillOnPlanSwapImmediate` (→ `creditGranter.Grant`)

The customer **paid gross** (net + tax) for that unused slice. Crediting only the net is a **latent cash-short bug**: a net credit balance offsets a *future gross* invoice 1:1, so the customer ends up short by the tax slice on any taxed invoice — and the tenant's upstream output-tax liability is never reduced, so it over-reports/over-remits relative to what it actually keeps.

Industry (Stripe / Chargebee / Lago) handles this with a **credit note** that reverses proportional tax against the original invoice. Velox already has that primitive (`creditnote.Service`), and the **unpaid**-prebill cancel branch (`relieveUnpaidPrebillOnCancel`) already uses it correctly. The three paths above are the inconsistent ones.

A tempting shortcut — adding a tax column to the credit ledger and grossing up the grant — is **wrong**: the ledger has no tax breakdown and is consumed as a pure cash offset on `amount_due`, with no place to file an authority-side reversal. A gross-on-the-ledger grant would refund the customer's tax as cash with **no** offsetting reversal → the tenant over-remits. Tax reversals belong on the **credit-note** document, anchored to the original invoice's committed `tax_transaction_id`.

## Decision

Route all three clawbacks through the existing tax-reversing **credit-note** primitive (`creditnote.Service.CreateAndIssueAdjustment`), against the **resolved original paid `in_advance` invoice** (already looked up at every site via `FindBaseInvoiceForPeriod`):

1. **Gross up** the net unused amount to the invoice's gross via its own `Total/Subtotal` ratio (`money.RoundHalfToEven(net × Total ÷ Subtotal)`; `1×` for zero-tax) — identical to `relieveUnpaidPrebillOnCancel`.
2. `CreateAndIssueAdjustment(src.ID, grossUnused, reason, desc)`. On a **paid** source invoice, `Issue` credits the full gross to the customer's **balance** (same spendable `CreditGrant` ledger entry as today) **and** reverses the proportional output tax against the original `tax_transaction_id` (no-op for manual/none providers — gated on `tax_transaction_id != ""`). The tax slice is derived from the **original invoice ratio** (`inv.TaxAmountCents × subtotal ÷ inv.TotalAmountCents`) with a last-CN residual true-up, so tax-inclusive pricing and floor() residuals are handled by construction and repeated clawbacks can't over-reverse (bounded by the per-invoice over-credit cap).
3. **Replace** the bare net grant at each site (never add alongside it — that would double-credit). The **unpaid**-prebill branch (`relieveUnpaidPrebillOnCancel`) is left untouched (it reduces `amount_due`, grants no balance, and must not become a balance-crediting CN).
4. Invoice **display**: present the upgrade proration as the two-line credit-unused-old + charge-new shape (the structurally tax-correct view), with plain-English labels.

When the credit-note adjuster is unwired (narrow unit tests only — production always wires it), fall back to the legacy net ledger grant so existing tests pass.

## Why this is correct (verified)

Cash-neutral and tax-correct, and strictly better than today: the customer gets back the **gross** they paid for the unused slice (which offsets a future gross invoice 1:1), and the tenant's authority-side liability is reduced by exactly the reversed tax — two separate ledgers, no double-count (the reversal never touches the credit ledger or `amount_due`).

## Consequences

- A clawback now produces a **credit note** (a tax document) instead of a bare ledger grant. Operator-visible artifact changes; the customer's spendable balance outcome is preserved (and corrected upward by the tax they overpaid).
- Each clawback CN must be anchored to the resolved source invoice id so the over-credit cap + cumulative tax true-up see prior clawbacks.
- Rolled out in phases: (A) engine cancel + plan-swap — **shipped** (#185); (B) downgrade proration — **shipped** (#186); (C) the two-line upgrade display — **shipped** (#187). Tracked in #184 (closed).

## Phase B notes (downgrade / quantity-decrease / item-remove)

The subscription handler gained a narrow `CreditNoteIssuer` dependency (`*creditnote.Service` satisfies it directly — same method as `billing.CreditNoteAdjuster`, no adapter). The downgrade-credit branch grosses the net clawback up by the resolved paid source invoice's `Total/Subtotal` ratio and issues the tax-reversing CN against it; the legacy bare-net `GrantProration` remains only as the fallback when the issuer is unwired (narrow tests) or no paid source invoice was resolved.

The CN is issued **after** the atomic item-change tx commits — `creditnote.Service` is not tx-aware and its tax reversal is an external Stripe call, so it can't ride the DB tx (a rollback would orphan a committed CN + balance grant). This mirrors the post-commit tax-commit established in #183.

**Dedup without a schema migration** (this is where the plan changed from the original "relocate the proration-source dedup onto the CN"): a client retry of the same downgrade is rejected at `Service.UpdateItem`'s no-op gate (the item is already on the new plan / new quantity), so the CN is never issued twice. The source invoice's per-invoice over-credit cap is the hard backstop against any aggregate over-credit. No new column / index was needed. The residual crash window (item committed, CN not yet issued, retry blocked by the no-op gate) leaves the customer un-credited until reconciliation — logged at error level; same risk class as #183's post-commit tax commit, accepted rather than carrying a heavier tx-aware credit-note refactor pre-launch.

## Phase C notes (two-line upgrade display)

A mid-cycle **plan upgrade** invoice now presents the structurally-correct two-line shape every reference platform converges on (Stripe / Recurly / Chargebee / Orb): a **negative** credit "Unused time on `<old plan>` (after `<date>`)" plus a **positive** charge "Remaining time on `<new plan>` (after `<date>`)", summing to the same net the single line used to show. A single "$18.00 plan upgrade proration" line is opaque ("I upgraded to $50 — why $18?"); the credit-unused + charge-remaining pair shows the arithmetic.

Key decisions:

1. **Display/structure only — the invoice total is provably unchanged.** Tax is still computed **once on the net** via `ApplyTaxToLineItems`, then the stored lines are partitioned *after* the tax call. The invoice `subtotal_cents` / `tax_amount_cents` / `total_amount_cents` are taken from the same `taxResult` as before, byte-for-byte.
2. **Never hand a signed line to the tax provider.** Stripe Tax rejects a negative line `Amount` (the invoice would spuriously defer to draft) and the manual provider clamps a negative base to zero (mis-apportioning the tax). So the split runs strictly *after* the single-net-line tax call. The credit line carries the **negative** reversed-tax slice (`round(T × credit ÷ net)`), the charge line the residual (`T − creditTax`) — `creditCents + chargeCents == net` and `creditTax + chargeTax == T` exactly, by deriving the charge halves as residuals rather than independently rounding.
3. **Scope = real plan change.** Add-item and quantity changes share the `proratedCents > 0` branch but have no distinct old plan to name as "unused time on …", so they keep the single net line. Gated on `change_type == plan`.
4. **Inclusive-tax fallback.** When inclusive-mode tax carves the net (`taxResult.SubtotalCents != proratedCents`), the split is skipped and the single net line stays — avoids any subtotal/line mismatch in the rare inclusive case.
5. **No new line type, no migration.** Both lines stay `base_fee`; `invoice_line_items.amount_cents` has no `>= 0` CHECK, so the negative credit line persists fine. Both `formatCents` (web + PDF) already sign-handle negatives; the PDF/web per-jurisdiction rollups only render for multi-jurisdiction invoices, and the web rollup's skip was aligned to `=== 0` (was `<= 0`) so a negative-tax line is included and the rows still sum to `invoice.tax_amount_cents` — mirroring the PDF's `== 0`.

## Amendment (2026-06-15): clawback credits fan across ALL invoices that funded the period

A period prepaid by **more than one** invoice (the day-1 base invoice **plus** a mid-period upgrade / quantity-increase proration invoice) broke the original single-source assumption. The cancel / cross-interval plan-swap credit is sized off the post-change base — correct, and equal to the unused prepayment across *both* invoices — but it was issued as one credit note against the single invoice `FindBaseInvoiceForPeriod` resolves, overran that invoice's per-invoice credit-note cap, hard-errored, and was swallowed to a silent $0 (a customer overcharge). See the 2026-06-15 money-bug audit.

- **Resolution:** a new `invoice.FindFundingInvoicesForPeriod` returns the full funding set (base-fee-line predicate ∪ the `subscription_update` invoice's header — `billing_reason` + `source_plan_changed_at ∈ period` + `billing_period_end`, all already-populated columns, **no migration**). `billing.settleUnusedAcrossFunding` partitions the **one authoritative** figure across the set with `money.AllocateByWeight` (largest-remainder); each piece is grossed up and tax-reversed against **its own** invoice's `tax_transaction_id`. Per-source reversal is *strictly more* ADR-048-correct than one blended reversal when the base and upgrade invoices carry different rates.
- **Partition, never independent recompute.** The shares partition the engine total rather than being recomputed per invoice — otherwise upgrade→downgrade→cancel double-counts (credits $80 for $40 of real unused). The over-credit guard is structural: the parts sum to the authoritative total by construction.
- **Fail loud.** A share that still can't be issued returns a hard error; `subscription.Service.Cancel` / the swap path log it at `slog.Error` ("customer not credited; manual credit required") instead of the prior silent warning, while keeping the terminal cancel/swap non-blocking on a transient credit failure.
- **Void / mark-uncollectible** reverse only the **remaining** un-credit-noted tax (`amount_paid + amount_due`), not a blind full reversal, so an invoice that already had adjustment credit notes isn't double-reversed (output-tax under-remittance).
- **Downgrade-clawback leg (done — distinct attribution):** unlike cancel/swap's *uniform* clawback of all unused prepayment, a downgrade/qty-decrease/item-removal claws back only the *removed* value, so it attributes per change type: a **plan downgrade** reverses **LIFO** (newest funding invoice first — undo the latest price level against its own tax, leaving the still-active base untouched; `money.AllocateLIFO`); **fungible** quantity/item changes split **proportionally** (`money.AllocateByWeightCapped`). Each piece caps at its invoice's remaining headroom; `ProrationDetail.ClawbackPieces` carries them and `issueClawbackCreditNote` issues one credit note per piece. Resolution is read-only inside the atomic item-change tx, so headroom exhaustion **rolls the change back pre-commit** rather than the post-commit reconcile gap. Industry-anchored (Stripe flexible mode reverses the original charge; per-charge credit memo is the dominant downgrade model).
- **Headroom-awareness (done):** the cancel/swap fan-out now caps each share at *remaining* creditable (`creditnote.Service.CreditedCents`) and water-fills overflow, so a **downgrade then cancel** doesn't overrun the invoice the downgrade already credited.

## The FUNDING-SET invariant (made explicit 2026-06-15)

The whole multi-invoice class above existed because one cardinality lived only as the single-row return shape of `FindBaseInvoiceForPeriod`, never as a named contract — so when this ADR (two-line upgrades / immediate proration invoicing) made a period fundable by **more than one** invoice, the readers (cancel / swap / void / downgrade) silently inherited the stale assumption. Now stated as a rule and enforced:

> **A subscription period can be funded by multiple invoices** (a base invoice + any mid-period upgrade / quantity-increase proration invoice). Any code computing a **period-wide credit, clawback, or void** MUST reconcile against the full set via `invoice.FindFundingInvoicesForPeriod` — never a single-row lookup.

- **Enforcement:** `scripts/lint-funding-set.sh` (CI: `make lint-funding-set`) fails on any `FindBaseInvoiceForPeriod` / `GetByProrationSource` call in the proration decision packages without a same/preceding-line `// single-invoice-ok: <reason>`. The legitimate single-row uses today are the paid/unpaid **routing gate** (the per-invoice math still fans across the funding set) and the proration-**dedup replay** (an idempotency lookup).
- **Funding set excludes usage:** threshold / usage / manual invoices are *not* funding — they bill consumed usage, never prepaid base, and so are never clawed back. The resolver excludes them via the `line_type='base_fee'` + period-start predicate.
- **Related tax invariant (void/uncollectible):** **tax is reversed only by credit notes.** Payments and applied customer credit reduce `amount_due` without reversing tax, so `reverseInvoiceTax` computes the remaining un-reversed gross as `total − Σ(non-voided credit notes)` (`CreditNoteTotaler`), not from `amount_due`.
