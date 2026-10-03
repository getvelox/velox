# ADR-057: Downgrade/removal clawback credit note — created atomically, issued recoverably

**Date:** 2026-06-20
**Status:** Accepted

## Summary

A downgrade, item removal or quantity decrease against a paid invoice gives unused prepayment back as a clawback credit note. On the atomic path, that note is now created as a `draft` with `issue_pending=true` inside the item-change transaction, so a create failure rolls the item change back. The non-atomic fallback still creates and issues the note inline. The note is issued after commit, and `RetryPendingClawbackIssue` re-issues any pending draft on the scheduler tick. The in_advance mid-period cancel credit uses the same pattern, but only when every funding source is paid; there, a draft-create failure rolls back the whole cancel. If any source is unpaid, the whole cancel credit stays on the older post-commit path. Amended 2026-06-25: ADR-061 made `Issue()` atomic, so a failure partway through issuing is now safe to retry.

## Context

Hands-on testing of the subscription RemoveItem flow (2026-06-20) confirmed a money gap the atomic-proration work (ADR-030, ADR-056) had left open on the **credit** side.

On a mid-cycle **downgrade / item-removal / quantity-decrease** against a PAID source invoice, the engine claws back the unused prepayment as a **tax-reversing adjustment credit note** (ADR-048), one per funding invoice (`ClawbackPieces`). On the atomic path, the item change ran inside a coordinator-owned `*sql.Tx` (`atomicRemoveItemWithProration` / `atomicUpdateItemWithProration`), but the clawback credit note was created **and** issued **post-commit, fire-and-forget**:

```
tx: RemoveItemTx + handleItemProration(detail)   // item change + clawback DETAIL, in-tx
tx.Commit()
issueClawbackCreditNote(detail)   // CreateAndIssueAdjustment per piece — POST-COMMIT, error only logged
```

`issueClawbackCreditNote` swallowed the error (`slog.Error "…customer not yet credited, reconcile manually"`). So a `credit_notes` write failure — or a crash in the post-commit window — **removed the item but silently never credited the customer.** No rollback, no retry: money owed *to the customer*, dropped. Empirically confirmed by revoking `INSERT ON credit_notes` and running RemoveItem: it returned `204`, deleted the item, and created **no** credit note.

Asymmetry: the **charge** side (upgrades / qty-increases) was already atomic — the proration *invoice* is created in-tx. Only the **credit** side was exposed. `atomicRemoveItemWithProration`'s docstring ("a credit failure rolls back the delete") was aspirational, not true.

**Constraint.** The clawback's `Issue()` files a **Stripe Tax reversal** (an external network call) and applies the customer-balance credit — it cannot run inside the DB tx (a rollback would orphan a committed reversal). So "just move it in-tx" is impossible for the whole operation. This is the same shape the invoice tax-commit already solved: durable DB state in-tx, the Stripe call post-commit + recoverable (`RetryPendingTaxCommit`).

## Decision

Split the clawback across the transaction boundary — **create in-tx, issue post-commit, recover via a reconciler** — mirroring the tax-commit pattern.

1. **Create the credit note as a DRAFT inside the item-change tx.** `createClawbackDraftsTx` → `creditnote.CreateAdjustmentDraftTx` → `store.CreateUnderInvoiceLockTx` on the coordinator's `*sql.Tx` (no nested Begin/Commit; the per-invoice advisory lock rides the caller's tx). A create failure returns an error, so the deferred rollback **undoes the item change** — the item change and the clawback obligation are all-or-nothing. The draft is marked `issue_pending` (new `credit_notes.issue_pending`, migration 0121).
2. **Issue post-commit.** `issueClawbackDrafts` issues the recorded draft ids after the tx commits. On failure the note stays `status='draft' AND issue_pending=true` — a durable marker, not a silent loss.
3. **Reconcile the *Issue-never-ran* case.** `creditnote.RetryPendingClawbackIssue` (wired into the scheduler tick via the new `ClawbackRetrier` hook, beside `RetryPendingTaxCommit`) re-issues pending drafts cross-tenant (scoped by livemode). The scan requires `status='draft' AND issue_pending`, so it recovers exactly the case where `Issue()` **never ran** (a crash/transient error in the post-commit window before `Issue()` flipped the status). That re-issue is safe by construction — nothing has applied yet, so `Issue()` runs fresh.

`issue_pending` is the discriminator so the reconciler never auto-issues **operator-created** drafts (they default `false`). Migration 0121 is additive (constant `DEFAULT false` → metadata-only on PG 11+) plus a partial index `(tenant_id, livemode) WHERE issue_pending AND status='draft'`.

The **non-atomic fallback** path (`handleItemProration` with `tx == nil`, used when `h.db` isn't wired) is unchanged — it still create+issues inline via `issueClawbackCreditNote`.

### Known gap: the post-flip partial-issue window (NOT closed here)

`Issue()` flips `status` draft→issued in its own committed tx **before** the side-effects (Stripe Tax reversal, balance credit). If a side-effect fails *after* that flip, the row is `status='issued'` — invisible to the reconciler's `status='draft'` scan. It is **not** auto-recovered; it surfaces only via a loud `ERROR` log and needs manual reconciliation. This change does **not** claim to close that window.

Closing it requires making `Issue()` fully re-entrant — the paid-source paths already dedup (tax reversal gated on `tax_transaction_id` + stable key; balance credit deduped by `source_credit_note_id`, migration 0093), but the **unpaid-source `ApplyCreditNote`** (amount_due reduction) is **not** idempotent, so a naive re-issue would double-reduce — plus scanning `issue_pending` regardless of status. That is a **tracked follow-up**, deliberately scoped out to keep the high-blast-radius shared `Issue()` path untouched in this change.

## Consequences

- A clawback **create** failure now rolls the item change back (true atomicity) — the customer is never removed-and-uncredited because a *create* failed. An **Issue-never-ran** failure (the common post-commit-window crash) is recovered automatically by the reconciler. A **post-flip partial issue** is the one residual gap (see above): durable + loudly logged, but manual to reconcile until the tracked follow-up lands.
- One new column (0121), one reconciler, one scheduler hook. No change to the charge path or the non-atomic fallback.
- **Deferred (flagged):** (a) the post-flip re-entrancy gap above. ~~(b) the void/uncollectible `reverseInvoiceTax` fire-and-forget reversal~~ — **shipped** (migration 0122): `RetryPendingTaxReversal` mirrors `RetryPendingTaxCommit`, re-driving a failed reversal off a self-clearing `invoices.tax_reversed_at` marker (idempotent at Stripe via the `inv_taxrev_<id>` reference); the inline failure log was raised to ERROR.
- The reconciler issues on wall-clock; for a clock-pinned (test-mode, `livemode=false`) draft this can mis-lane `issued_at` (cosmetic, the 0117 concern) but does not affect the money. Not bound per-draft in v1.

Guarded by the real-Postgres `TestCreateUnderInvoiceLockTx_RollsBackWithCallerTx` (create-atomicity) and the service-level `TestRetryPendingClawbackIssue` (reconciler recovers an Issue-never-ran draft).

## Extension: cancel-proration credit (2026-06-24)

The same pattern, applied to a sibling that was still on the pre-atomic path: the **in_advance cancel-proration credit** (the unused, already-paid portion handed back when a subscription is canceled mid-period).

**The gap.** `subscription.Service.Cancel` runs three phases: (1) `CancelAtomicWithBill` flips status→`canceled` **and** inserts the in_arrears final-on-cancel invoice in one tx (ADR-056); (2) `FinalizeOnCreateInvoice` post-commit; (3) **`BillOnCancel` post-commit, in no tx** — it computes the unused-portion credit and issues it via `settleUnusedAcrossFunding → CreateAndIssueAdjustment`. So the customer's *cash owed back* is a fire-and-forget step that runs *after* the cancel committed. Transient/cap failures are **loud** (ERROR "manual credit required"), but a **crash between the cancel commit and phase 3 silently loses the obligation** — no draft, no trace. This is the credit-direction sibling of the create/cancel revenue leaks (#306/#307); industry-confirmed as a real outlier (Stripe/Orb/Lago/Chargebee/Recurly all make the cancellation credit a durable, recoverable credit note, paid→credit-balance by default — which Velox already does, just not durably).

**The fix — a transactional outbox, reusing this ADR's primitives (no new migration, no new reconciler).** The durable obligation (the credit-note **draft**, `issue_pending`) is created **in the cancel tx**; the external effect (Stripe Tax reversal + balance grant) is relayed **post-commit** by `Issue()`; the relay's recovery is the existing `RetryPendingClawbackIssue`. The capability lives in the **engine** — not the service (no creditnote dep) and not the handler (it never owns the cancel tx; `CancelAtomicWithBill` exposes the tx only via its `billFn` callback). Engine-writes-on-the-subscription-store's-tx is the established `BillFinalOnImmediateCancelTx` precedent (the final invoice already inserts on that same tx), not new coupling.

- Widen the engine's `CreditNoteAdjuster` with `CreateAdjustmentDraftTx` + `Issue` (both already on `*creditnote.Service`).
- New `Biller.BillOnCancelDraftsTx(tx, sub) → (ids, handled)` creates the drafts **inside `CancelAtomicWithBill`'s `billFn`, after the final invoice** (the documented final-invoice-before-credit ordering is then structurally guaranteed); post-commit `Biller.IssueCancelDrafts(sub, ids)` issues them — mirroring `BillOnCreateTx`/`FinalizeOnCreateInvoice`.
- **Atomic-refuse on any draft-create failure** (behavior change, intentional): a failed `CreateAdjustmentDraftTx` — including a credit-note **cap rejection** from a concurrent operator CN shrinking the source's headroom *after* the off-tx `CreditedCents` read — rolls the **whole cancel** back. This is stricter than the pre-fix post-commit path (which logged + let the cancel proceed with the credit **stranded**); refusing-and-retrying is the more correct money behavior, and it **self-heals**: the operator's retry re-reads the smaller headroom and `allocate()` sizes a fitting share. A genuinely fully-credited source never reaches the cap — it's sized to zero at `allocate()` and skipped.
- `settleUnusedAcrossFunding` is split at the create boundary into a **pure `allocate()`** (weights, headroom caps, `AllocateByWeightCapped` water-fill, **net→gross clamp and the remainder loud-fail/WARN** — these MUST travel with `allocate()` or the #276/#277/#278 over-credit-partition invariant re-opens) and a thin consumer. The headroom read (`CreditedCents`) stays **off** the coordinator tx — `CreditedCents` counts non-voided notes *including drafts*, so an on-tx read would let the in-flight draft shrink its own headroom.

**Scope: PAID funding sources only (PR1), gated on an all-paid check.** Because `CreditedCents` counts drafts, the coupled allocator can't be run twice (a post-commit re-allocate would double-count the committed drafts). So PR1 takes the atomic in-tx path **only when every funding source is paid** (the dominant in_advance cancel); if *any* source is unpaid, it falls through to today's `BillOnCancel` unchanged. This deliberately leaves the **entire unpaid branch on the post-commit best-effort path** (with its existing loud ERROR): both `relieveUnpaidPrebill` sub-paths — the non-idempotent `amount_due` reduce *and* the fully-unused `invoiceVoider.Void` (which has **no credit-note / `issue_pending` representation**, so the reconciler can never recover it). A mixed paid+unpaid funding set therefore stays fully post-commit in PR1 — coherent, because the silent crash-loss being closed is *cash owed back* (paid sources), not receivable relief.

**Deferred to PR2 (this ADR's tracked follow-up):** move **settled** unpaid-source relief onto the in-tx `issue_pending` draft path, gated on an **idempotent unpaid-source `ApplyCreditNote`** (the same prerequisite as the partial-issue re-entrancy fix above — fold them together). The `Void` sub-path needs its own dedup'd-reversal representation, separate from `ApplyCreditNote` idempotency. In-flight unpaid sources are **already** recoverable via the ADR-059 deferral — do not re-open them.

**Inherited verbatim, not closed:** the post-flip partial-issue window (above) applies identically to the cancel drafts — a paid grant is `source_credit_note_id`-deduped (0093) so a manual re-drive is safe, but there is no *auto* resume once `Issue()` has flipped `status='issued'`. The durable close is the in-tx grant (ADR-061), a future PR.

## Amendment (2026-06-25, PR2 / ADR-061): the inherited gaps are CLOSED

ADR-061 shipped (PR2), making `creditnote.Issue()` atomic + recoverable. The two gaps this ADR documented as inherited-but-open are now closed:

- **"the unpaid-source `ApplyCreditNote` is not yet idempotent"** (the PR2 prerequisite this ADR named) — closed. The reduction now runs on `Issue()`'s coordinator `*sql.Tx` via `ApplyCreditNoteTx`, gated by the `draft→issued` CAS, so it is **idempotent by construction** (exactly one caller, the CAS makes a second application impossible). No source-dedup row was needed — ADR-061 records why, and where the dedup row re-enters (a second `amount_due` mutator).
- **the post-flip partial-issue re-entrancy window** — closed. The CAS and the internal effect (grant / reduce) now commit **together**: a crash mid-tx rolls back to `draft` (so `RetryPendingClawbackIssue` re-drives cleanly), and a crash post-commit leaves the effect already applied alongside the flip. There is no `status='issued'` row with an un-applied internal effect. The external legs (refund, tax reversal) recover independently (`RetryRefund`, `RetryPendingCreditNoteTaxReversal`).

Consequently the `IssueCancelDrafts` post-commit relay (the cancel path's phase 3) is now safe to re-drive on any failure — `engine.IssueCancelDrafts`'s log no longer claims a manual-reconcile gap (updated in PR2). The unpaid-source **completeness** item — moving the unpaid relief into a single cancel-and-credit tx (relaxing the all-paid gate, and the `Void` sub-path's own dedup'd-reversal representation) — remains deferred per this ADR's "Deferred to PR2" note; PR2 closed the *correctness* hole (`Issue()` re-entrancy), not that completeness gap.
