# Architecture Decision Records

This directory holds Velox's Architecture Decision Records (ADRs) —
short docs capturing one architectural decision each. Format follows
[Michael Nygard's 2011 convention](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions):
Title / Date / Status / Context / Decision / Alternatives / Consequences.
New ADRs also open with a short **Summary**, so a reader learns the
current rule without reading the whole record.

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
  line (e.g. `Accepted (amended YYYY-MM-DD)`); the reason goes in the
  amendment section, and the Summary is updated to the current rule.
- **Date** is when the ADR was first written. Amendment dates go in
  the status line and inline section headers.
- **Multi-platform claims** ("Stripe-parity", "industry standard")
  must quote verified source lines from at least 2-4 reference
  platforms. Single-platform spot-checks aren't research.
- New ADRs start from [TEMPLATE.md](TEMPLATE.md).
- **Deferred work** an ADR deliberately scopes out — with a written revisit
  trigger in its *Consequences* — gets a thin pointer row in
  [Open follow-ups](#open-follow-ups-deferred) below. The rationale stays in the
  ADR (no duplication → nothing to drift); the table only makes open deferrals
  discoverable in one place. Remove the row when the follow-up ships.

## Index

Each row links to the full record. Browse by Topic, or search this page for a term from the [glossary](../README.md#glossary).

| # | Date | Status | Topic | Decision |
|---|---|---|---|---|
| [001](001-paymentintent-only-stripe.md) | 2026-04-15 | Accepted | Payments | Charge through Stripe PaymentIntents only; Velox owns invoices, dunning and payment status |
| [002](002-per-domain-package-architecture.md) | 2026-04-15 | Accepted | Platform & ops | Each domain package owns its store, service and handler; peer domains never import each other |
| [003](003-postgresql-rls-multi-tenancy.md) | 2026-04-15 | Accepted | Auth & security | Every table carries `tenant_id`; Postgres Row-Level Security enforces tenant isolation |
| [004](004-event-sourced-credit-ledger.md) | 2026-04-15 | Accepted | Credits | Credits live in an append-only ledger; the balance comes from the latest entry, not a mutable column |
| [005](005-integer-cents-for-money.md) | 2026-04-15 | Accepted (amended) | Billing | Store and compute money as integer cents; per-unit rates later became decimals (ADR-045) |
| [006](006-background-scheduler-vs-message-queue.md) | 2026-04-15 | Accepted | Platform & ops | Run billing and dunning on an in-process ticker; Postgres row locks prevent double work |
| [007](007-revert-to-api-key-dashboard-auth.md) | 2026-04-29 | Superseded by ADR-011 | Auth & security | The dashboard logs in with a pasted API key kept in browser storage |
| [008](008-session-from-api-key.md) | 2026-04-29 | Superseded by ADR-011 | Auth & security | Dashboard sessions are httpOnly cookies minted from a validated API key |
| [009](009-invoice-attention.md) | 2026-04-30 | Accepted | Invoices | Every invoice payload carries one server-computed attention field: severity, reason, actions |
| [010](010-tenant-timezone-model.md) | 2026-05-01 | Accepted (amended) | Timezones | Store and send instants in UTC; show dates in the tenant's display timezone |
| [011](011-email-password-auth-and-clean-api-keys.md) | 2026-05-01 | Accepted (amended) | Auth & security | The dashboard signs in with email and password; API keys serve only SDK and curl callers |
| [012](012-day-grade-calendar-billing.md) | 2026-05-01 | Accepted | Billing | Billing periods start and end at midnight in the tenant timezone; a day is the smallest unit |
| [013](013-invoice-attention-collection-state.md) | 2026-05-01 | Accepted | Invoices | Attention reports a missing payment method apart from an invoice that is awaiting payment |
| [014](014-sso-direction-embedded-oidc-saml.md) | 2026-05-02 | Accepted | Auth & security | When SSO is needed, embed OIDC and SAML libraries in-process; keep passwords homegrown |
| [015](015-test-clock-async-catchup.md) | 2026-05-04 | Accepted | Test clocks & simulated time | Advancing a test clock returns at once; a background worker runs the catch-up |
| [016](016-test-clock-soft-delete.md) | 2026-05-04 | Superseded by ADR-086 | Test clocks & simulated time | Deleting a test clock soft-deletes it and cancels its pinned subscriptions |
| [017](017-tax-retry-and-auto-finalize.md) | 2026-05-04 | Accepted | Tax | A background job retries failed tax calculation and finalizes the invoice when it succeeds |
| [018](018-test-clock-retry-and-failure-reason.md) | 2026-05-04 | Accepted | Test clocks & simulated time | A failed test-clock advance stores its reason and can be retried |
| [019](019-stripe-connect-flushes-stuck-tax.md) | 2026-05-04 | Accepted | Tax | Connecting Stripe retries invoices stuck on a missing or unauthorised tax provider |
| [020](020-invoice-timeline-coalesce-and-card-detail.md) | 2026-05-04 | Accepted | Invoices | The server drops redundant provider rows from the invoice timeline and shows the charged card |
| [021](021-hosted-invoice-pay-flow-saves-pm.md) | 2026-05-03 | Accepted | Payments | Paying a hosted invoice saves the card to the customer for later off-session charges |
| [022](022-contextual-checkout-return-urls.md) | 2026-05-04 | Accepted | Payments | Stripe Checkout returns the operator to the page they started from, not a fixed URL |
| [023](023-post-decline-rendering-cleanup.md) | 2026-05-05 | Accepted | Payments | After a decline, show one banner and one email per moment; no email during on-page payment |
| [024](024-activity-timeline-design-deferred.md) | 2026-05-05 | Accepted | Invoices | Keep building the timeline from several sources; add new primitives only on a named trigger |
| [025](025-attention-detail-vs-provider-response.md) | 2026-05-05 | Accepted | Invoices | Attention keeps Velox's own explanation apart from the raw text a provider returned |
| [026](026-error-boundary-sanitization.md) | 2026-05-05 | Accepted | Platform & ops | One function turns errors into HTTP responses; only typed, safe messages pass through |
| [027](027-customer-level-test-clock.md) | 2026-05-05 | Accepted | Test clocks & simulated time | Test clocks attach to the customer; subscriptions inherit the customer's clock |
| [028](028-billing-engine-period-loop-and-disjoint-flows.md) | 2026-05-05 | Accepted | Billing | Bill every due period in one call; cron and test-clock catch-up bill separate subscriptions |
| [029](029-fully-disjoint-test-clock-flows.md) | 2026-05-08 | Accepted | Test clocks & simulated time | Every time-driven job has separate wall-clock and per-test-clock flows |
| [030](030-simulated-time-everywhere-on-clock-pinned-entities.md) | 2026-05-08 | Accepted | Test clocks & simulated time | Every action on a clock-pinned entity runs on the clock's simulated time |
| [031](031-per-plan-base-bill-timing.md) | 2026-05-14 | Accepted | Pricing & usage | Each plan bills its base fee in advance or in arrears; usage always bills in arrears |
| [032](032-public-cost-dashboard-projection.md) | 2026-05-14 | Accepted | Integrations | A token-protected public JSON endpoint shows one customer's costs, with sanitized fields |
| [033](033-litellm-spend-adapter.md) | 2026-05-14 | Accepted | Integrations | LiteLLM pushes each call's spend to a Velox endpoint, which records it as usage events |
| [034](034-plan-billing-field-immutability.md) | 2026-05-15 | Accepted | Pricing & usage | A plan's billing fields freeze once a live subscription uses it; names stay editable |
| [035](035-per-fact-simulated-time-anchoring.md) | 2026-05-16 | Accepted | Test clocks & simulated time | During test-clock catch-up, each record carries the simulated time its event happened |
| [036](036-dunning-campaigns-model.md) | 2026-05-16 | Accepted (amended) | Dunning | A tenant keeps several dunning policies, one default, and can assign a policy per customer |
| [037](037-trial-end-and-activation-period-anchoring.md) | 2026-05-18 | Accepted (amended) | Subscriptions | Shared helpers compute the first period after a trial or activation; trial changes are atomic |
| [038](038-credit-note-three-channel-allocation.md) | 2026-05-24 | Accepted | Credits | A credit note splits its amount across refund, credit balance and out-of-band channels |
| [039](039-cut-coupons-pre-launch.md) | 2026-05-30 | Accepted | Product scope | Remove coupons entirely before launch |
| [040](040-outbox-always-on.md) | 2026-05-30 | Accepted | Platform & ops | Webhooks and emails always go through the outbox; the on/off flags are removed |
| [041](041-tax-fallback-manual-removed.md) | 2026-05-30 | Accepted | Tax | If tax calculation fails, block the invoice; there is no fallback to manual tax |
| [042](042-tax-rate-decimal-precision.md) | 2026-05-31 | Accepted | Tax | Store tax rates as `NUMERIC(7,4)` and prorate with a whole-day ratio |
| [043](043-drop-tax-rate-bp.md) | 2026-05-31 | Accepted | Tax | Drop the basis-point tax rate column at once; the decimal rate is the only storage |
| [044](044-canonical-ai-token-metering-model.md) | 2026-06-01 | Accepted | Pricing & usage | Meter all AI tokens on one `tokens` meter, with token type, model and provider as dimensions |
| [045](045-decimal-per-unit-pricing-rates.md) | 2026-06-01 | Accepted | Pricing & usage | Per-unit price rates are arbitrary-precision decimals; invoice amounts stay integer cents |
| [046](046-manual-tax-largest-remainder-apportionment.md) | 2026-06-03 | Accepted | Tax | Manual tax rounds the document total once and spreads the cents by largest remainder |
| [047](047-invoice-tax-rate-displays-statutory-not-effective.md) | 2026-06-05 | Accepted | Tax | The invoice shows the statutory tax rate when taxed lines share one; else the effective rate |
| [048](048-credit-clawback-tax-reversal.md) | 2026-06-06 | Accepted | Credits | Credit clawbacks go through a credit note, so the proportional tax is reversed too |
| [049](049-payment-settlement-primitive.md) | 2026-06-07 | Accepted | Payments | One idempotent settle step owns every Stripe charge's paid or failed outcome and its side effects |
| [050](050-unpaid-source-proration-policy.md) | 2026-06-08 | Accepted | Subscriptions | While the period invoice is unpaid, block a change that charges more; adjust one that credits |
| [051](051-remove-customer-self-serve-portal.md) | 2026-06-09 | Accepted | Product scope | Remove the customer self-serve portal; it is a B2C pattern our users do not need |
| [052](052-customer-tax-status-engine-determined-vs-override.md) | 2026-06-15 | Accepted | Tax | The tax engine decides tax status by default; manual reverse-charge or exempt overrides it |
| [053](053-explicit-payment-method-at-charge.md) | 2026-06-17 | Accepted | Payments | Velox picks the card to charge and names it on the charge; Stripe never chooses |
| [054](054-effective-unit-price-decimal-display.md) | 2026-06-17 | Accepted | Invoices | Show per-unit prices at full precision; one backend function picks the billed or the effective rate |
| [055](055-anniversary-month-end-anchor.md) | 2026-06-18 | Accepted | Billing | Anniversary billing keeps the original day of month, clamped to each month's last day |
| [056](056-atomic-cross-interval-plan-swap.md) | 2026-06-19 | Accepted | Subscriptions | A plan swap across billing intervals restructures the cycle in one transaction |
| [057](057-atomic-recoverable-downgrade-clawback.md) | 2026-06-20 | Accepted | Credits | A downgrade's clawback credit note is created in the change transaction and issued with retry |
| [058](058-billing-date-math-tenant-timezone.md) | 2026-06-09 | Accepted | Timezones | All month and year billing date math is anchored in the tenant timezone |
| [059](059-guard-invoice-mutations-while-payment-in-flight.md) | 2026-06-22 | Accepted | Payments | In-flight payments block void, offline payment and write-off (parked excepted); clawbacks wait |
| [060](060-no-payment-method-dunning-enrollment.md) | 2026-06-23 | Accepted | Dunning | Invoices with no payment method enter dunning, like declined charges |
| [061](061-credit-note-issue-atomicity.md) | 2026-06-24 | Accepted (amended) | Credits | Issuing a credit note commits internal effects in one transaction; external effects retry |
| [062](062-async-obligation-backbone.md) | 2026-06-25 | Accepted | Platform & ops | Move the retry sweeps onto one separate in-database obligations queue; the build is deferred |
| [063](063-refund-status-webhook-reconciliation.md) | 2026-06-28 | Accepted | Payments | Stripe webhooks decide refund status; the status at creation is recorded as Stripe returned it |
| [064](064-dunning-run-creation-derived-from-invoice-state.md) | 2026-07-02 | Accepted | Dunning | A decline starts a dunning run at once; a background sweep creates any run that was missed |
| [065](065-threshold-scan-boundary-fire-once-drain.md) | 2026-07-02 | Accepted | Billing | A usage threshold fires at most once per scan, never at the period end, and drains fully |
| [066](066-threshold-money-semantics.md) | 2026-07-02 | Accepted | Billing | A threshold fire bills a prorated base and resets the cycle in the same transaction |
| [067](067-archive-blocks-never-cancels.md) | 2026-07-02 | Accepted | Subscriptions | Archiving a customer is refused while a subscription still bills; it never cancels anything |
| [068](068-checkout-session-dedup.md) | 2026-07-02 | Accepted | Payments | Record a checkout session before creating it in Stripe, so an invoice has one live session |
| [069](069-trial-cancel-semantics.md) | 2026-07-02 | Accepted | Subscriptions | Cancelling in a trial is free at trial end; SQL guards enforce it at every activation |
| [070](070-price-change-semantics.md) | 2026-07-02 | Accepted (amended) | Pricing & usage | Customer price overrides follow the rule across versions; a period keeps its opening price |
| [071](071-credit-expiry-retires-the-block.md) | 2026-07-03 | Accepted | Credits | An expired credit grant is fully retired in the same transaction as its expiry entry |
| [072](072-transport-lease-model.md) | 2026-07-03 | Accepted | Platform & ops | Outbox workers claim rows with a time-limited lease; budgets and lease lengths are derived |
| [073](073-selfhost-boot-and-bootstrap-contract.md) | 2026-07-03 | Accepted | Platform & ops | One bootstrap path creates a tenant in one transaction; migrations run one at a time |
| [074](074-subscription-billing-timezone-snapshot.md) | 2026-07-04 | Superseded by ADR-077 | Timezones | Each subscription snapshots its billing timezone at creation |
| [075](075-canonical-utc-api-timestamps.md) | 2026-07-04 | Accepted | Timezones | The process runs in UTC, so every API timestamp is the same whatever the host zone |
| [076](076-enforcing-the-timezone-invariant.md) | 2026-07-04 | Accepted | Timezones | A CI lint and typed helpers enforce that dates render in an explicit timezone |
| [077](077-org-level-billing-timezone.md) | 2026-07-04 | Accepted | Timezones | The billing timezone is one org-level setting; subscriptions carry no timezone of their own |
| [078](078-prepaid-commits-phase-1.md) | 2026-07-05 | Accepted | Credits | A prepaid commit is a credit grant funded when its invoice finalizes and retired on void |
| [079](079-provider-cost-tables.md) | 2026-07-05 | Accepted | Pricing & usage | Record what the operator pays LLM providers per token and report margin per customer |
| [080](080-paid-commit-cn-relief.md) | 2026-07-06 | Accepted | Credits | A refund of unused paid commit credits is capped by the price paid per credit |
| [081](081-minimal-team-invites.md) | 2026-07-06 | Accepted | Auth & security | Invite teammates by single-use email link; removal revokes sessions; roles are not enforced |
| [082](082-email-recipient-semantics.md) | 2026-07-06 | Accepted | Platform & ops | Customers can hold extra encrypted email addresses; a fixed matrix decides who is CC'd |
| [083](083-recipe-adoption-conformance-gate.md) | 2026-07-07 | Superseded by ADR-085 | Pricing & usage | A recipe adopts an existing plan or meter only if its billing config matches; else it refuses |
| [085](085-recipe-idempotent-apply.md) | 2026-07-08 | Accepted | Pricing & usage | Applying a recipe is additive: a first apply makes a new plan, a repeat creates nothing, no uninstall |
| [086](086-simulated-data-lifecycle.md) | 2026-07-09 | Accepted (amended) | Test clocks & simulated time | Money sweeps skip simulated invoices; deleting a clock deletes its customers' rows, not the audit log |
| [087](087-collect-after-finalize-pipeline.md) | 2026-07-11 | Superseded by 116 | Payments | One method collects payment after every engine finalize; each site keeps its own checks |
| [088](088-credit-balance-applies-to-all-invoices.md) | 2026-07-11 | Accepted | Credits | The credit balance applies to every invoice at finalize; the card is charged the remainder |
| [089](089-retire-audit-fail-closed-response-swap.md) | 2026-07-13 | Accepted | Audit | A failed audit write never changes the API response; it is logged and counted |
| [090](090-audit-in-tx-emission.md) | 2026-07-13 | Accepted | Audit | Target: audit rows commit in the business transaction; only some writers do, most still write after commit |
| [091](091-org-timezone-change-seam-absorb.md) | 2026-07-14 | Accepted | Timezones | Changing the org timezone never overbills; the period gap it creates is absorbed or prorated |
| [092](092-split-billing-timezone-from-display.md) | 2026-07-14 | Proposed | Timezones | Split the billing timezone from the display timezone; deferred until a partner needs it |
| [093](093-csrf-origin-verification.md) | 2026-07-16 | Accepted | Auth & security | State-changing cookie requests must come from the same site, checked by origin headers |
| [094](094-login-brute-force-throttle.md) | 2026-07-17 | Accepted (amended) | Auth & security | v1 has no login lockout; a throttle, MFA and breached-password check are designed, deferred |
| [095](095-login-security-roadmap-and-mfa.md) | 2026-07-17 | Accepted (amended) | Auth & security | Ship TOTP MFA and an offline breached-password check before first deployment |
| [096](096-unpriced-meter-plan-attach-guard.md) | 2026-07-18 | Accepted | Pricing & usage | Refuse to attach an unpriced meter to a plan; log loudly if one still reaches finalize |
| [097](097-mid-period-scheduled-cancel-fires-as-immediate-cancel.md) | 2026-07-18 | Accepted | Subscriptions | A scheduled cancel that falls mid-period fires as an immediate cancel at that time |
| [098](098-postmark-delivery-webhook-ingestion.md) | 2026-07-19 | Accepted | Integrations | Ingest Postmark delivery, bounce and spam-complaint webhooks; ignore opens and clicks |
| [099](099-simulated-time-disclosure-policy.md) | 2026-07-21 | Accepted | Test clocks & simulated time | Mark simulated data once per scope with the word "Simulated": a page banner or a list chip |
| [100](100-currency-coherence-guard-ring.md) | 2026-07-26 | Accepted | Pricing & usage | Reject mismatched currencies at every write; the engine never converts |
| [101](101-billing-intervals.md) | 2026-07-27 | Accepted | Subscriptions | Store each item's billable date ranges on change; billing intersects them with the period |
| [102](102-invoice-charge-attempts.md) | 2026-07-28 | Accepted | Payments | Record each charge attempt as a row, updated by PaymentIntent id as Stripe reports |
| [103](103-single-payment-source.md) | 2026-07-28 | Accepted | Payments | The invoice timeline shows payments only from charge-attempt rows, not from the webhook table |
| [104](104-one-activity-lane-entity-calendar.md) | 2026-07-29 | Accepted | Test clocks & simulated time | Activity is one list ordered by the entity's own time, with the real time as a subline |
| [105](105-charge-idempotency-key-seed.md) | 2026-07-30 | Accepted | Payments | Seed the Stripe idempotency key from a per-invoice attempt counter, not `updated_at` |
| [106](106-charge-intent-ledger.md) | 2026-07-30 | Parked | Payments | Record each charge before calling Stripe, so an unnamed charge can be replayed safely |
| [107](107-unknown-is-terminal-until-a-human.md) | 2026-07-31 | Accepted | Payments | A charge Stripe cannot name stays unknown until a webhook or a human resolves it |
| [108](108-parked-invoices-search-and-adopt.md) | 2026-08-02 | Accepted | Payments | Resolve unknown charges by searching Stripe, and act only on a payment actually found |
| [109](109-issued-document-snapshot.md) | 2026-08-05 | Proposed (amended) | Invoices | Copy bill-to and supplier details onto the invoice when it leaves draft; render from the copy |
| [110](110-written-off-invoices-close-self-service-payment.md) | 2026-08-05 | Accepted | Invoices | A written-off invoice can no longer be paid online by the customer; its pages point to support |
| [111](111-a-write-off-has-no-tax-leg.md) | 2026-08-05 | Accepted | Tax | Writing off an invoice does not reverse its tax; voids and credit notes still do |
| [112](112-dunning-exhaustion-settles-two-questions.md) | 2026-08-05 | Accepted | Dunning | When dunning ends, decide the subscription's outcome and the invoice's outcome separately |
| [113](113-nothing-charges-a-written-off-invoice.md) | 2026-08-06 | Accepted | Invoices | Nothing charges a written-off invoice; it settles only by a recorded payment or a found charge |
| [114](114-leader-leases-tick-scoped-fencing.md) | 2026-08-30 | Accepted | Platform & ops | Leader election is a database row per role, with a fencing token checked by every claim |
| [115](115-one-closer-for-the-billing-period.md) | 2026-08-30 | Accepted | Billing | Every period write locks the subscription and rechecks its period snapshot first |
| [116](116-collection-intent-at-finalize-one-collector.md) | 2026-10-08 | Accepted | Payments | Finalize queues the invoice in its own write; one collector charges it, and a nudge runs it right away |

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
| **Money-email in-tx** (payment receipt, payment failed, dunning warning, dunning escalation) — all four are enqueued post-commit via `EnqueueStandalone`; a process death or DB failover between the state transition's commit and the enqueue's commit loses the email while the state stands, and the money path's exactly-once gates suppress every later attempt. **Scheduled (2026-08-30 HA program, after the ha-8 CAS precursor):** atomic-first — the state-transition store method takes an in-tx continuation invoked only on the transition branch, the caller resolves recipient + suppression BEFORE the transition, and the email side gains `Send*Tx(ctx, tx, …)` variants; the old marker+reconciler shape is retired (reconciler is a fallback, never the primary). Operator-initiated sends (invoice sent, credit note) stay post-commit — no state transition, synchronous error to the operator. `payment_setup_request` already has a reconciler (auto-charge sweep re-sends while `NoPMNotifiedAt` is nil). | ADR-040 amendment 2026-08-30 |
| **Stale-deferred-draft alarm** — Part B (ADR-059) defers an automated clawback against an in-flight source until the charge settles. If the charge *never* settles (a wedged `requires_action` PI nobody authenticates/cancels), the deferred draft waits unissued. It is **not lost** (durably captured, auto-issues on settle) and a wedged payment is independently visible (stuck `processing` invoice, tenant unpaid), so this is an *observability* gap, not a correctness one: surface a draft deferred > N days so an operator can cancel/await the PI (which auto-resolves the clawback). | [059](059-guard-invoice-mutations-while-payment-in-flight.md) §Deferred | `internal/creditnote/service.go` · `RetryPendingClawbackIssue` | operability hardening / first ACH-SEPA design partner |
| **`amount_paid` edge — Part C: record from captured** — `MarkPaid` records `amount_paid = amount_due` at settle. Under Velox's PaymentIntent-only **full-capture** model this equals the captured amount, so it is **not reachable today** (Velox exposes no partial/manual-capture flow). If partial capture is ever added, record from the PI's `amount_received` (the processor's captured amount) instead. | [059](059-guard-invoice-mutations-while-payment-in-flight.md) §Consequences | `internal/invoice/postgres.go` · `MarkPaid` | partial-capture support |
| **Per-customer balance_low threshold** — `credit.balance_low` fires only on the single tenant-level threshold; consumers cannot implement per-customer low alerts from it (falsified 2026-07-06 — the design doc's consumer-side mitigation was impossible). Workaround: smallest-customer tenant threshold + always-per-customer `balance_depleted` + polling `GET /v1/credits/grants/{id}`. | [078](078-prepaid-commits-phase-1.md) §D8 (amended) | `internal/credit/postgres.go` · `emitBalanceCrossings` | first DP with heterogeneous commit sizes asking for per-customer low alerts |
| **`DispatchTx` seam — atomic lifecycle-event emission (remaining perimeter)** — the **subscription lifecycle subset SHIPPED 2026-07-05**: `subscription.created` / `.activated` / `.canceled` / `.trial_ended` are enqueued IN their transition txs at the store level (`subscription.PostgresStore.SetOutboxEnqueuer`, one emit site per transition covering the operator API + engine schedule paths + trial sweeps; the ~10 scattered post-commit dispatch sites were deleted). The settlement money events (`invoice.paid`, `payment.succeeded`) were already in-tx. `dunning.started` joined them 2026-10-08 (ADR-116: `dunning.Store.StartRun` writes run + timeline event + webhook in one tx). **Remaining**: the ~12 other notification events (item/pending-change/collection/dunning/invoice.finalized/voided etc.) still dispatch post-commit — a dropped enqueue leaves no row anywhere and the loss is silent (there is no consumer-reconciliation mechanism). Per-service `dispatchEvent` ERROR-logs failures. | dual-write audit (no ADR) + 2026-07-05 reassessment | `internal/subscription/postgres.go` · `enqueueLifecycle` (shipped) / `internal/domain/webhook_outbound.go` · `EventDispatcher` (rest) | first DP integration that consumes a non-subscription lifecycle event |

### Resolved follow-ups

- Clawback post-flip partial-issue window: resolved by ADR-061 (#313). See [057](057-atomic-recoverable-downgrade-clawback.md) and [061](061-credit-note-issue-atomicity.md).
- Cross-interval swap refund lost or double-credited on crash-retry: resolved 2026-07-05 (#381). See [056](056-atomic-cross-interval-plan-swap.md).
- `SettleFailed` event, email and dunning in-tx, plus dunning recovery: resolved. See [064](064-dunning-run-creation-derived-from-invoice-state.md).
- Commit credit-note retire leg: resolved 2026-07-06. See [080](080-paid-commit-cn-relief.md).

## Writing a new ADR

```
cp TEMPLATE.md NNN-short-slug.md
```

Pick NNN as the next sequential number — check `ls docs/adr/`, NOT
your local branch (per `feedback_migration_numbering`: numbering is
chosen from origin/main to avoid duplicates that only fail at
integration-test time).
