# Architecture Decision Records

This directory holds Velox's Architecture Decision Records (ADRs) —
short docs capturing one architectural decision each. Format follows
[Michael Nygard's 2011 convention](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions):
Title / Date / Status / Context / Decision / Alternatives / Consequences.

ADRs are written when a decision is **worth re-litigating later** —
either because reasonable alternatives exist, because the constraints
behind the choice will likely shift, or because the decision shapes
multiple downstream design choices. They are **not** prose summaries
of every change; routine refactors and bug fixes belong in commit
messages + CHANGELOG.md, not here.

## Conventions

- **Numbering** is sequential; never re-used. Superseded ADRs stay in
  the directory with their `**Status:** Superseded by ADR-XXX` line —
  the historical context is the whole point.
- **Status** is one of: `Proposed`, `Accepted`, `Superseded by ADR-XXX`,
  `Deprecated`. Amendments to an accepted ADR are noted in the status
  line (e.g. `Accepted (amended YYYY-MM-DD — short reason)`).
- **Date** is when the ADR was first written. Amendment dates go in
  the status line and inline section headers.
- **Multi-platform claims** ("Stripe-parity", "industry standard")
  must quote verified source lines from at least 2-4 reference
  platforms (per `feedback_verify_stripe_parity_claims` in
  `~/.claude/.../memory/`). Single-platform spot-checks aren't research.
- New ADRs start from [TEMPLATE.md](TEMPLATE.md).
- **Deferred work** an ADR deliberately scopes out — with a written revisit
  trigger in its *Consequences* — gets a thin pointer row in
  [Open follow-ups](#open-follow-ups-deferred) below. The rationale stays in the
  ADR (no duplication → nothing to drift); the table only makes open deferrals
  discoverable in one place. Remove the row when the follow-up ships.

## Index

| # | Date | Status | Title |
|---|---|---|---|
| [001](001-paymentintent-only-stripe.md) | 2026-04-15 | Accepted | PaymentIntent-Only Stripe Integration |
| [002](002-per-domain-package-architecture.md) | 2026-04-15 | Accepted | Per-Domain Package Architecture |
| [003](003-postgresql-rls-multi-tenancy.md) | 2026-04-15 | Accepted | PostgreSQL Row-Level Security for Multi-Tenancy |
| [004](004-event-sourced-credit-ledger.md) | 2026-04-15 | Accepted | Event-Sourced Credit Ledger |
| [005](005-integer-cents-for-money.md) | 2026-04-15 | Accepted | Integer Cents for Money |
| [006](006-background-scheduler-vs-message-queue.md) | 2026-04-15 | Accepted | Background Scheduler vs. Message Queue |
| [007](007-revert-to-api-key-dashboard-auth.md) | 2026-04-29 | **Superseded by ADR-011** | Revert Dashboard to API-Key Auth |
| [008](008-session-from-api-key.md) | 2026-04-29 | **Superseded by ADR-011** | Dashboard Session Cookies Minted From API Keys |
| [009](009-invoice-attention.md) | 2026-04-30 | Accepted | Unified Invoice Attention Surface |
| [010](010-tenant-timezone-model.md) | 2026-05-01 | Accepted | Tenant Timezone Model |
| [011](011-email-password-auth-and-clean-api-keys.md) | 2026-05-01 | Accepted | Email/Password Dashboard Auth, Pure-CRUD API Keys |
| [012](012-day-grade-calendar-billing.md) | 2026-05-01 | Accepted | Day-Grade Calendar Billing & Period Boundary Snapping |
| [013](013-invoice-attention-collection-state.md) | 2026-05-01 | Accepted | Invoice Attention — Honest Collection-State Surface |
| [014](014-sso-direction-embedded-oidc-saml.md) | 2026-05-02 | Accepted | SSO Direction — Embedded OIDC/SAML, No SaaS Auth Vendor |
| [015](015-test-clock-async-catchup.md) | 2026-05-04 | Accepted | Test-clock advance runs catchup asynchronously |
| [016](016-test-clock-soft-delete.md) | 2026-05-04 | Superseded by 086 | Test clocks were soft-deleted with cascade-cancel of pinned subs; now a hard DELETE with FK-cascade teardown |
| [017](017-tax-retry-and-auto-finalize.md) | 2026-05-04 | Accepted | Background tax-retry reconciler + auto-finalize on success |
| [018](018-test-clock-retry-and-failure-reason.md) | 2026-05-04 | Accepted | Test-clock retry advance + persisted failure reason |
| [019](019-stripe-connect-flushes-stuck-tax.md) | 2026-05-04 | Accepted | Stripe re-connect flushes stuck tax invoices |
| [020](020-invoice-timeline-coalesce-and-card-detail.md) | 2026-05-04 | Accepted | Invoice timeline coalesces redundant rows and surfaces the charged card |
| [021](021-hosted-invoice-pay-flow-saves-pm.md) | 2026-05-03 | Accepted | Hosted-invoice Pay flow saves the card to the customer's Stripe customer |
| [022](022-contextual-checkout-return-urls.md) | 2026-05-04 | Accepted | Stripe Checkout return URLs are contextual, not global |
| [023](023-post-decline-rendering-cleanup.md) | 2026-05-05 | Accepted | Post-decline rendering — one banner, distinct emails, interactive suppression |
| [024](024-activity-timeline-design-deferred.md) | 2026-05-05 | Accepted | Activity timeline — keep multi-source assembly; promote canonical primitives only on trigger |
| [025](025-attention-detail-vs-provider-response.md) | 2026-05-05 | Accepted | Attention banner separates Velox's classification from upstream provider responses |
| [026](026-error-boundary-sanitization.md) | 2026-05-05 | Accepted | HTTP error responses are sanitized at a single boundary |
| [027](027-customer-level-test-clock.md) | 2026-05-05 | Accepted | Test clocks attach at the customer level (Stripe parity) |
| [028](028-billing-engine-period-loop-and-disjoint-flows.md) | 2026-05-05 | Accepted | Billing engine — per-sub period loop + disjoint flows for catchup vs cron |
| [029](029-fully-disjoint-test-clock-flows.md) | 2026-05-08 | Accepted | Fully disjoint test-clock flows across every time-aware engine path |
| [030](030-simulated-time-everywhere-on-clock-pinned-entities.md) | 2026-05-08 | Accepted | Simulated time everywhere on clock-pinned entities |
| [031](031-per-plan-base-bill-timing.md) | 2026-05-14 | Accepted | Per-plan base bill_timing (in_advance vs in_arrears) |
| [032](032-public-cost-dashboard-projection.md) | 2026-05-14 | Accepted | Public cost-dashboard projection — token shape + sanitization contract |
| [033](033-litellm-spend-adapter.md) | 2026-05-14 | Accepted | LiteLLM spend adapter — wedge integration |
| [034](034-plan-billing-field-immutability.md) | 2026-05-15 | Accepted | Plan billing-field immutability once a live sub attaches |
| [035](035-per-fact-simulated-time-anchoring.md) | 2026-05-16 | Accepted | Per-fact simulated-time anchoring under test-clock catchup |
| [036](036-dunning-campaigns-model.md) | 2026-05-16 | Accepted (amended 2026-05-16) | Dunning campaigns model (multi-policy-per-tenant) |
| [037](037-trial-end-and-activation-period-anchoring.md) | 2026-05-18 | Accepted (amended 2026-05-18) | Trial-end and activation period anchoring (centralized helpers + Phase 0.5/0.9 trial-expiry) |
| [038](038-credit-note-three-channel-allocation.md) | 2026-05-24 | Accepted | Credit notes use three explicit allocation channels (Stripe + Lago shape) |
| [039](039-cut-coupons-pre-launch.md) | 2026-05-30 | Accepted | Cut coupons pre-launch |
| [040](040-outbox-always-on.md) | 2026-05-30 | Accepted | Outbox is the only path (webhook + email) |
| [041](041-tax-fallback-manual-removed.md) | 2026-05-30 | Accepted | Remove `tax_on_failure=fallback_manual` (block-only) |
| [042](042-tax-rate-decimal-precision.md) | 2026-05-31 | Accepted | Tax-rate decimal precision + proration integer day-ratio |
| [043](043-drop-tax-rate-bp.md) | 2026-05-31 | Accepted | Drop `tax_rate_bp` immediately (no transition window) |
| [044](044-canonical-ai-token-metering-model.md) | 2026-06-01 | Accepted | Canonical AI token-metering model (one `tokens` meter + `token_type` dimension) |
| [045](045-decimal-per-unit-pricing-rates.md) | 2026-06-01 | Accepted | Decimal per-unit pricing rates + multi-dim cycle billing |
| [046](046-manual-tax-largest-remainder-apportionment.md) | 2026-06-03 | Accepted | Manual tax — document-level total + largest-remainder line apportionment |
| [047](047-invoice-tax-rate-displays-statutory-not-effective.md) | 2026-06-05 | Accepted | Invoice-level `tax_rate` displays the statutory rate, not the effective rate |
| [048](048-credit-clawback-tax-reversal.md) | 2026-06-06 | Accepted | Credit clawbacks reverse proportional tax via the credit-note primitive |
| [049](049-payment-settlement-primitive.md) | 2026-06-07 | Accepted | A single payment-settlement primitive (discover-then-settle) |
| [050](050-unpaid-source-proration-policy.md) | 2026-06-08 | Accepted | Unpaid-source proration — block charges, adjust credits |
| [051](051-remove-customer-self-serve-portal.md) | 2026-06-09 | Accepted | Remove the customer self-serve portal |
| [052](052-customer-tax-status-engine-determined-vs-override.md) | 2026-06-15 | Accepted | Customer tax status — engine-determined by default, manual flag is the override |
| [053](053-explicit-payment-method-at-charge.md) | 2026-06-17 | Accepted | Charge the explicit payment method — Velox owns PM selection |
| [054](054-effective-unit-price-decimal-display.md) | 2026-06-17 | Accepted | Display the per-unit price at full precision — derive on read, don't store |
| [055](055-anniversary-month-end-anchor.md) | 2026-06-18 | Accepted (amends ADR-058 date-math) | Anniversary billing clamps to month-end from a persisted anchor day |
| [056](056-atomic-cross-interval-plan-swap.md) | 2026-06-19 | Accepted | Cross-interval plan swap restructures the cycle atomically |
| [057](057-atomic-recoverable-downgrade-clawback.md) | 2026-06-20 | Accepted | Downgrade/removal clawback credit note — created atomically, issued recoverably |
| [058](058-billing-date-math-tenant-timezone.md) | 2026-06-09 | Accepted | Billing calendar date-math is anchored in the tenant timezone (renumbered from a duplicate 050) |
| [059](059-guard-invoice-mutations-while-payment-in-flight.md) | 2026-06-22 | Accepted | Guard invoice mutations while a payment is in flight — block void/uncollectible/offline-record; single void writer; defer the automated clawback until the source settles |
| [060](060-no-payment-method-dunning-enrollment.md) | 2026-06-23 | Accepted | Card-less invoices enter dunning (no-payment-method enrollment) |
| [061](061-credit-note-issue-atomicity.md) | 2026-06-25 | Built (PR2) | Credit-note `Issue()` atomicity — CAS + internal money effect in one coordinator tx; external effects post-commit + recoverable |
| [062](062-async-obligation-backbone.md) | 2026-06-25 | Decided (design); build deferred (River premise corrected 2026-08-30) | Async obligation backbone — generalise the outbox (not Temporal/River/pgx-now); consolidate the four re-drive sweeps; trigger-gated |
| [063](063-refund-status-webhook-reconciliation.md) | 2026-06-28 | Accepted | Refund status is reconciled from Stripe webhooks (async truth); create-time status recorded faithfully; monotonic terminal-wins |
| [064](064-dunning-run-creation-derived-from-invoice-state.md) | 2026-07-02 | Accepted | Dunning-run creation — triggered-primary + derived-backstop; run existence & schedule are derivable (self-heals by construction), NOT a single-mechanism scalar; full-derive rejected |
| [065](065-threshold-scan-boundary-fire-once-drain.md) | 2026-07-03 | Accepted | Threshold-scan kernel — boundary-skip, fire-once probe, full drain |
| [066](066-threshold-money-semantics.md) | 2026-07-03 | Accepted | Threshold money semantics — prorated reset base, atomic fire→reset, $0 auto-pay, max/last deferral |
| [067](067-archive-blocks-never-cancels.md) | 2026-07-03 | Accepted | Archive blocks new business — it never cancels, and it never lies |
| [068](068-checkout-session-dedup.md) | 2026-07-03 | Accepted | Checkout session dedup — claim-first ledger, choke-point closes, loud anomaly settlement |
| [069](069-trial-cancel-semantics.md) | 2026-07-03 | Accepted | Trial-cancel semantics — free at trial end, guarded in SQL at all four activation writers |
| [070](070-price-change-semantics.md) | 2026-07-03 | Accepted | Price-change semantics — overrides follow rule_key, periods pin at open |
| [071](071-credit-expiry-retires-the-block.md) | 2026-07-03 | Accepted | Credit expiry retires the block — atomically, and retirement wins |
| [072](072-transport-lease-model.md) | 2026-07-03 | Accepted | Outbox transport — claim-lease with detached CAS marks, derived constants |
| [073](073-selfhost-boot-and-bootstrap-contract.md) | 2026-07-03 | Accepted | Self-host boot contract — one bootstrap writer, honored APP_DATABASE_URL, serialized hybrid migrations |
| [074](074-subscription-billing-timezone-snapshot.md) | 2026-07-04 | Superseded by 077 | A subscription's billing timezone is snapshotted at creation, not read live |
| [075](075-canonical-utc-api-timestamps.md) | 2026-07-04 | Accepted | The process runs in UTC so API timestamps are canonical, not host-dependent |
| [076](076-enforcing-the-timezone-invariant.md) | 2026-07-04 | Accepted | The timezone-display invariant is enforced, not just documented |
| [077](077-org-level-billing-timezone.md) | 2026-07-04 | Accepted | The billing timezone is an org-level setting, not a per-subscription snapshot |
| [078](078-prepaid-commits-phase-1.md) | 2026-07-05 | Accepted | Prepaid commits + drawdown, phase 1 — grant-on-issue, retire-on-void, cash-instrument invoices |
| [079](079-provider-cost-tables.md) | 2026-07-05 | Accepted | Provider cost tables + per-customer margin — ingest-time COGS stamp, honest attribution |
| [080](080-paid-commit-cn-relief.md) | 2026-07-06 | Accepted | Paid-commit CN relief — telescoping price-ratio refund cap, single-tx create-and-issue |
| [081](081-minimal-team-invites.md) | 2026-07-06 | Accepted | Minimal team invites — tokenized accept, session-revoking removal, NO RBAC (role recorded, not enforced) |
| [082](082-email-recipient-semantics.md) | 2026-07-06 | Accepted | Email recipients — encrypted additional_emails, CC coverage matrix (credentials never CC), CN send endpoint |
| [083](083-recipe-adoption-conformance-gate.md) | 2026-07-08 | Superseded | Recipe adoption conformance gate — refuse to adopt a divergent plan/meter, never silently |
| [085](085-recipe-idempotent-apply.md) | 2026-07-11 | Accepted | A recipe is an idempotent, additive provisioning event — one verb, no uninstall |
| [086](086-simulated-data-lifecycle.md) | 2026-07-09 | Accepted | Simulated-data lifecycle — durable `is_simulated` sweep-gates + clock-delete teardown |
| [087](087-collect-after-finalize-pipeline.md) | 2026-07-11 | Accepted | One post-finalize collection pipeline; collection gates stay per-site |
| [088](088-credit-balance-applies-to-all-invoices.md) | 2026-07-11 | Accepted | Customer credit balance applies to every invoice at finalize, card charged the remainder |
| [089](089-retire-audit-fail-closed-response-swap.md) | 2026-07-13 | Accepted | Retire the audit fail-closed response swap (and the `audit_fail_closed` setting) |
| [090](090-audit-in-tx-emission.md) | 2026-07-13 | Accepted | In-transaction audit emission (LogInTx) and the declared-coverage model |
| [091](091-org-timezone-change-seam-absorb.md) | 2026-07-14 | Accepted | An org-timezone change never overbills a subscription — absorb/prorate the re-anchor "seam" |
| [092](092-split-billing-timezone-from-display.md) | 2026-07-14 | Proposed | Split the org billing timezone from the org display timezone (DEFERRED design) |
| [093](093-csrf-origin-verification.md) | 2026-07-16 | Accepted | CSRF defense for the cookie dashboard — origin verification, not tokens |
| [094](094-login-brute-force-throttle.md) | 2026-07-17 | Accepted | Login brute-force protection — Postgres-authoritative, non-weaponizable, extensible |
| [095](095-login-security-roadmap-and-mfa.md) | 2026-07-17 | Accepted | Login-security roadmap & MFA design — minimal-complete for a self-hostable B2B money dashboard |
| [096](096-unpriced-meter-plan-attach-guard.md) | 2026-07-18 | Accepted | Unpriced meter on a plan — guard at authoring, loud at finalize |
| [097](097-mid-period-scheduled-cancel-fires-as-immediate-cancel.md) | 2026-07-18 | Accepted | A due mid-period `cancel_at` fires as an immediate cancel at that instant |
| [098](098-postmark-delivery-webhook-ingestion.md) | 2026-07-19 | Accepted | Postmark delivery/bounce/complaint webhook ingestion |
| [099](099-simulated-time-disclosure-policy.md) | 2026-07-21 | Accepted | Simulated-time disclosure — once per scope, one word, banner teaches / chip flags |
| [100](100-currency-coherence-guard-ring.md) | 2026-07-26 | Accepted | Currency coherence is enforced at every write — a guard ring, not engine reconciliation |
| [101](101-billing-intervals.md) | 2026-07-27 | Accepted | Billing intervals — write-time item lifetimes replace read-time log interpretation |
| [102](102-invoice-charge-attempts.md) | 2026-07-28 | Accepted | Charge attempts as first-class facts |
| [103](103-single-payment-source.md) | 2026-07-28 | Accepted | One owner for payment rows on the invoice timeline |
| [104](104-one-activity-lane-entity-calendar.md) | 2026-07-29 | Accepted | One activity lane — the entity's calendar |
| [105](105-charge-idempotency-key-seed.md) | 2026-07-30 | Accepted | The charge idempotency key is seeded by an attempt counter, not `updated_at` |
| [106](106-charge-intent-ledger.md) | 2026-07-31 | Superseded | Record the charge attempt before calling Stripe |
| [107](107-unknown-is-terminal-until-a-human.md) | 2026-07-31 | Accepted | An unnameable charge attempt parks the invoice; it is never settled failed |
| [108](108-parked-invoices-search-and-adopt.md) | 2026-08-02 | Accepted | Parked invoices are resolved by provider search — adopt what you can name, never settle absence |
| [110](110-written-off-invoices-close-self-service-payment.md) | 2026-08-05 | Accepted | A written-off invoice closes the customer's self-service payment route |
| [111](111-a-write-off-has-no-tax-leg.md) | 2026-08-05 | Accepted | A write-off has no tax leg |
| [112](112-dunning-exhaustion-settles-two-questions.md) | 2026-08-05 | Accepted | Dunning exhaustion settles two questions, not one |
| [113](113-nothing-charges-a-written-off-invoice.md) | 2026-08-06 | Accepted | Nothing charges a written-off invoice — recording writers only; recovery runs on normal rails |

> ℹ️ **ADR-084 was never used.** No file has ever carried that number (verified
> across every ref). It is a skipped number, not a lost decision.
>
> ⚠️ **This index sat frozen at 082 for a month** while ADRs 083–112 shipped —
> 28 decisions, including the whole written-off-invoice arc (110/111/112),
> were undiscoverable here. Backfilled 2026-08-05; dates for the 20 whose
> `**Status:**` line carried no date come from each file's first commit.
> Nothing enforces this table, so it rots silently: add the row in the same PR
> as the ADR.

> ℹ️ **ADR-058 was renumbered from a duplicate ADR-050** (2026-06-21). Two
> concurrent sessions had each taken `050` — the same hazard the migration-numbering
> rule guards against (pick the next number from origin/main, not a local branch).
> The earlier-dated 050 (`unpaid-source-proration`, 2026-06-08) kept the number;
> the later (`date-math`, 2026-06-09) became **058** and all its references were
> updated. The out-of-date-order number is expected for a renumber.

## Open follow-ups (deferred)

Work an ADR deliberately scoped out, as **thin pointers** — the authoritative
rationale and revisit trigger live in each ADR's *Consequences/Deferred* section;
this table only makes the open items discoverable in one place. Remove a row when
its follow-up ships. (These share one shape: a post-commit side-effect / external
call not yet in-tx or idempotent. None is a regression; each is guarded today and
gated on a named trigger — see `feedback_pre_launch_scoping`.)

| Follow-up | ADR | Code site | Revisit trigger |
|---|---|---|---|
| ~~Clawback **post-flip partial-issue** window~~ — **RESOLVED by ADR-061 (#313).** `Issue()` is now a coordinator tx: the draft→issued CAS and the internal money effect commit together, so a side-effect failure can no longer strand an `issued`-but-unapplied row invisible to the reconciler; external legs each have their own retry (refund → `RetryRefund`, tax reversal → `RetryPendingCreditNoteTaxReversal`). (Row was stale for weeks — flagged by the 2026-07-05 deferral reassessment.) | [057](057-atomic-recoverable-downgrade-clawback.md) → [061](061-credit-note-issue-atomicity.md) | `internal/creditnote/service.go` · `Issue` | — |
| ~~**Bug B** — cross-interval swap refund lost/double-credited on crash-retry~~ — **RESOLVED 2026-07-05 (#381).** Both re-arm triggers had fired; the refund now commits as `issue_pending` drafts ON the swap tx (`BillOnPlanSwapDraftsTx`, the BillOnCancelDraftsTx transplant), post-commit Issue self-heals via the clawback reconciler, and the dashboard item dialogs send `Idempotency-Key`. Unpaid-funding declines to the legacy post-commit relief (cancel-path contract). | [056](056-atomic-cross-interval-plan-swap.md) §Consequences | `internal/subscription/service.go` · `applyCrossIntervalPlanSwapTx` | — |
| **Money-email in-tx** (payment receipt, payment failed, dunning warning, dunning escalation) — all four are enqueued post-commit via `EnqueueStandalone`; a process death or DB failover between the state transition's commit and the enqueue's commit loses the email while the state stands, and the money path's exactly-once gates suppress every later attempt. **Scheduled (2026-08-30 HA program, after the ha-8 CAS precursor):** atomic-first — the state-transition store method takes an in-tx continuation invoked only on the transition branch, the caller resolves recipient + suppression BEFORE the transition, and the email side gains `Send*Tx(ctx, tx, …)` variants; the old marker+reconciler shape is retired (reconciler is a fallback, never the primary). Operator-initiated sends (invoice sent, credit note) stay post-commit — no state transition, synchronous error to the operator. `payment_setup_request` already has a reconciler (auto-charge sweep re-sends while `NoPMNotifiedAt` is nil). | ADR-040 amendment 2026-08-30 |
| ~~**`SettleFailed` event/email/dunning in-tx + dunning recovery**~~ — **RESOLVED.** All three halves closed: `payment.failed` is enqueued **in-tx** by `MarkPaymentFailedReportingTransition` (gated on `firstForThisPI`, symmetric to the success-path `payment.succeeded` fold); dunning recovery is the `dunning_backfill` reconciler (`Engine.EnrollFailedWithoutDunning`, #328, 0085-exactly-once); the failed-**email** stays post-commit **by design** (symmetric to the receipt email — folding it in-tx would drag customer-email + suppression reads under the invoice row lock). See ADR-064 for the run-creation architecture. | settlement.go · ADR-064 | `internal/invoice/postgres.go` · `MarkPaymentFailedReportingTransition` | — |
| **Stale-deferred-draft alarm** — Part B (ADR-059) defers an automated clawback against an in-flight source until the charge settles. If the charge *never* settles (a wedged `requires_action` PI nobody authenticates/cancels), the deferred draft waits unissued. It is **not lost** (durably captured, auto-issues on settle) and a wedged payment is independently visible (stuck `processing` invoice, tenant unpaid), so this is an *observability* gap, not a correctness one: surface a draft deferred > N days so an operator can cancel/await the PI (which auto-resolves the clawback). | [059](059-guard-invoice-mutations-while-payment-in-flight.md) §Deferred | `internal/creditnote/service.go` · `RetryPendingClawbackIssue` | operability hardening / first ACH-SEPA design partner |
| **`amount_paid` edge — Part C: record from captured** — `MarkPaid` records `amount_paid = amount_due` at settle. Under Velox's PaymentIntent-only **full-capture** model this equals the captured amount, so it is **not reachable today** (Velox exposes no partial/manual-capture flow). If partial capture is ever added, record from the PI's `amount_received` (the processor's captured amount) instead. | [059](059-guard-invoice-mutations-while-payment-in-flight.md) §Consequences | `internal/invoice/postgres.go` · `MarkPaid` | partial-capture support |
| **Per-customer balance_low threshold** — `credit.balance_low` fires only on the single tenant-level threshold; consumers cannot implement per-customer low alerts from it (falsified 2026-07-06 — the design doc's consumer-side mitigation was impossible). Workaround: smallest-customer tenant threshold + always-per-customer `balance_depleted` + polling `GET /v1/credits/grants/{id}`. | [078](078-prepaid-commits-phase-1.md) §D8 (amended) | `internal/credit/postgres.go` · `emitBalanceCrossings` | first DP with heterogeneous commit sizes asking for per-customer low alerts |
| ~~**Commit CN-retire leg**~~ — **RESOLVED 2026-07-06 (ADR-080).** Paid-commit relief shipped: telescoping price-ratio cap (refund = `f(K+r)−f(K)` with `f(k)=RoundHalfToEven(GrossPaid·k, Granted)`), single-tx create-and-issue with the retire in the same tx, credit channel structurally forbidden, offline-paid defaults to out_of_band. Remaining scope cuts with named triggers live in ADR-080 (mixed-line invoices; dispute modeling). | [080](080-paid-commit-cn-relief.md) | `internal/creditnote/service.go` · `CreateAndIssueCommitRelief` | — |
| **`DispatchTx` seam — atomic lifecycle-event emission (remaining perimeter)** — the **subscription lifecycle subset SHIPPED 2026-07-05**: `subscription.created` / `.activated` / `.canceled` / `.trial_ended` are enqueued IN their transition txs at the store level (`subscription.PostgresStore.SetOutboxEnqueuer`, one emit site per transition covering the operator API + engine schedule paths + trial sweeps; the ~10 scattered post-commit dispatch sites were deleted). The settlement money events (`invoice.paid`, `payment.succeeded`) were already in-tx. **Remaining**: the ~12 other notification events (item/pending-change/collection/dunning/invoice.finalized/voided etc.) still dispatch post-commit — a dropped enqueue leaves no row anywhere and the loss is silent (there is no consumer-reconciliation mechanism). Per-service `dispatchEvent` ERROR-logs failures. | dual-write audit (no ADR) + 2026-07-05 reassessment | `internal/subscription/postgres.go` · `enqueueLifecycle` (shipped) / `internal/domain/webhook_outbound.go` · `EventDispatcher` (rest) | first DP integration that consumes a non-subscription lifecycle event |

## Writing a new ADR

```
cp TEMPLATE.md NNN-short-slug.md
```

Pick NNN as the next sequential number — check `ls docs/adr/`, NOT
your local branch (per `feedback_migration_numbering`: numbering is
chosen from origin/main to avoid duplicates that only fail at
integration-test time).
