# ADR-087: One post-finalize collection pipeline; collection gates stay per-site

**Status:** Superseded by [ADR-116](116-collection-intent-at-finalize-one-collector.md) (2026-10-08). Collection now runs in one collector, and every finalize queues the invoice in its own write. The per-site gates and the flag-on-decline analysis below are history.
**Originally accepted:** 2026-07-11
**Context:** design review 2026-07-10, redesign #4 ("CollectAfterFinalize"); executed after an 8-agent site census verified 18 divergence dimensions across the seven finalize paths.

## Decision

Extract the post-finalize collection **pipeline** — resolve payment setup →
charge (30s, reload-first), or queue `auto_charge_pending` + send the
setup-link email — into one method, `Engine.collectAfterFinalize`, called by
the four engine finalize sites (cycle close, subscription_create day-1,
final-on-cancel, finalized threshold fire).

Do **not** unify the per-site collection **gates**. Credit-apply with
`creditApplyOK` failure-suppression, pause-collection, `amount_due > 0`, and
the threshold's draft/finalized status split remain at their call sites.

Do **not** fold in the three non-engine paths:

- **Manual finalize** (`invoice.Handler.collectAtFinalize`) — deliberately
  different retry-owner semantics (no flag on decline; the charged invoice is
  returned in the HTTP response). Its verified drift was a recorded follow-up,
  not silently absorbed — **executed 2026-07-11 as #448–#451**: PM-ID added to
  the readiness predicate (#448); charge failures classified — definite
  declines stay dunning-owned/no-flag, transient/unknown/unclassified queue
  for the sweep, safe by the sweep's payment_status='pending' predicate
  (#449); collection detached from the caller's cancellation in BOTH collect
  steps + engine-parity 30s charge deadline (#450 — the engine pipeline's
  day-1/final-on-cancel callers arrive on request ctxs too); zero-due settles
  PAID via Service.SettleZeroDue, the manual writer's ADR-066 T12 twin
  (#451).
- **Proration invoices** — sweep-mediated by design: inline charging would
  wall-clock-charge simulated invoices and duplicate the charge path
  (subscription handler comment). The missing no-PM setup email was **closed
  2026-07-11 (#453)** in the sweep, generalized to every sweep-mediated
  card-less invoice: send-once via the durable `invoices.no_pm_notified_at`
  marker (migration 0145), stamped by every sender (finalize-time collect
  steps + the sweep) so no path duplicates another; resolve errors still send
  nothing (unknown ≠ missing); skipped-no-email stays unstamped so it
  self-heals when the customer gains an address.
- **Tax-retry auto-finalize** — collectless by design; the threshold path
  pre-plants the inert flag on tax-deferred drafts and the sweep collects the
  moment tax retry finalizes.

## Why the gates stay per-site (the traps a full unification hits)

1. **Credit-apply presence is a revenue-behavior decision.** Cycle close and
   threshold apply the customer's credit balance and suppress both the charge
   and the $0-MarkPaid arm when the apply fails (`creditApplyOK` — the
   2026-05-30 overcharge fix). Day-1 / final-on-cancel / manual do not apply
   credits. Adopting credit-apply there changes what customers are charged —
   a deliberate product decision to make explicitly, never a refactor
   side-effect.
2. **The $0/fully-credited MarkPaid arm is entangled with cycle control
   flow.** In billOnePeriod it advances the billing period and early-returns;
   extracting it either double-advances or skips the advance. Its
   `Status == Finalized` conjunct is the DEMO-000906 guard (a tax-pending
   draft must never transition to paid).
3. **Decline retry-ownership differs on purpose.** Engine sites flag
   `auto_charge_pending=true` on decline while the charger starts dunning
   inline; the sweep clears the flag for decline codes so dunning is the
   single owner. Manual finalize never flags. Unifying either direction
   creates double-charge windows (two retry owners, distinct idempotency
   keys) or silent dead-ends. A shared decline arm needs error-type
   classification, which no finalize site had at decision time; #449 has
   since given the MANUAL site exactly that classification (declines →
   dunning, everything else → flag) — the engine sites keep flag-always.

   **Engine flag-on-decline: analyzed and deliberately KEPT (2026-07-11).**
   Ownership is partitioned by `payment_status`: a persisted decline is
   `failed`, which the sweep's pending-only predicate never lists — the flag
   is inert and dunning is the single owner. The only entrance to a
   two-owner state (`pending` + flag + active run) is a decline whose
   outcome-persist fails all 3 retries (verified: `startDunningWithRetry`
   fires after the persist attempt regardless, and nothing ever resets
   `payment_status` to `pending`). In that state the interleavings converge:
   first success marks paid and the other owner's pre-check skips; a sweep
   decline clears the flag (ownership handoff). Actual double capture needs
   same-instant ticks from two independent schedulers on top of the triple
   failure, and lands in the payment-anomaly machinery (#318–#320), not
   silently. Removing flag-on-decline to "fix" that tail would orphan the
   sibling case — persist failed AND inline dunning start failed AND webhook
   lost leaves `pending` with NO owner but the flag. The flag is the safety
   net for exactly the failure that creates the window; classification at
   engine sites would be net-harm. The two stale "dunning is NOT started
   inline" comments (engine sweep + stripe.go) that obscured this analysis
   are fixed alongside this note.

   **Long-term designs, recorded with triggers (2026-07-11).** The root
   structural fact behind the tail: the Stripe call and the local outcome
   write cannot be atomic, and the outcome is recorded only AFTER the call —
   a lost write makes the ownership partition lie. Two successors, in
   ascending order of cost:

   - **Tier 1 — derived ownership (adopt on next touch of the sweep or
     dunning-enrollment queries).** Add "AND no ACTIVE dunning run" (EXISTS)
     to the sweep's list + claim predicates. Single-owner becomes mechanical
     even in the torn state, while the rescue property survives (no run ⇒
     the sweep still owns the orphan). Derivability precedent: ADR-064;
     query precedent: ListFailedWithoutDunningRun. ~Half a day with
     collision tests; strictly dominates the current design and retires this
     section's analysis burden.
   - **Tier 2 — charge-attempt ledger (trigger: production cutover / first
     design partner with real money, or the FIRST payment_anomaly firing in
     anger).** Persist an attempt row (invoice, idempotency key,
     state=started) BEFORE calling Stripe; persist the outcome into it
     after; a reconciler re-derives stuck 'started' attempts from Stripe by
     the STORED key/PI. Kills the torn-write class entirely (a lost outcome
     write becomes discoverable instead of invisible), replaces the fragile
     UpdatedAt-derived idempotency key with a stored one (stable against
     interleaved row writes), and closes the residual DB-outage-during-
     persist window that the #450 ctx-detach could not. Deliberately NOT
     built pre-launch: real machinery (migration + hot-path writes + a
     reconciler across every charge site) with zero observed anomalies —
     the pre-launch scoping bar says a named pressure first.

## Error-path fixes unified into the pipeline (behavior changes, tested)

- Resolver error ≠ no-PM: queue for the sweep, do **not** email — before this,
  all four sites emailed "payment method needed" to card-having customers on a
  transient read error.
- Pre-charge reload failure queues for the sweep — before this it skipped
  silently (no charge, no flag, no log): the invoice never entered any retry
  path.
- Pre-notify reload failure is logged (was silent email loss).

The charger/paymentSetups/noPMNotifier nil-guards at the four sites are
deleted (boot fails closed on nil collaborators since #442 — this completes
the redesign #3 arm-deletion program for the collect gates).

## Consequences

- The #434-style "edit the notify outcome switch at four sites" class is gone;
  pipeline changes are one edit.
- Site log lines are now prefixed by a site tag ("cycle close", …) instead of
  four hand-written variants.
- Unit fixtures reaching a finalize path must wire the collect collaborators;
  `wireBaseTax` defaults them (no-PM readiness, recording notifier, erroring
  sentinel charger).
