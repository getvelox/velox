# ADR-036: Dunning campaigns model (multi-policy-per-tenant)

**Date:** 2026-05-16
**Status:** Accepted (amended 2026-05-16 — terminal-action set + pause-semantics fix; amended 2026-07-05 — initial-default-at-first-create + no-policy = deliberate skip; **the terminal-action half is SUPERSEDED by [ADR-112](112-dunning-exhaustion-settles-two-questions.md), 2026-08-05**; see Amendment sections below)

> ⚠️ **The 2026-05-16 amendment below describes a `final_action` column that no
> longer exists.** [ADR-112](112-dunning-exhaustion-settles-two-questions.md)
> split it into `final_subscription_action` (`none`/`pause`/`cancel`) and
> `final_invoice_action` (`none`/`mark_uncollectible`), because three of the
> four values below describe the SUBSCRIPTION and one describes the INVOICE —
> so choosing one meant giving up the others, and "cancel and write off" was
> inexpressible. The per-action *semantics* in that table are still accurate;
> only their mutual exclusivity is gone. Everything else in this ADR — the
> campaigns model, per-customer assignment, the retry schedule — stands.

## Context

The pre-2026-05-16 dunning data model was a tenant-wide singleton:

- `dunning_policies` enforced one row per `(tenant_id, livemode)` via a
  UNIQUE constraint. The "policy" was a tenant-global retry config:
  `(max_retry_attempts, grace_period_days, retry_schedule[],
  final_action)`.
- `customer_dunning_overrides` allowed a per-customer **partial-field
  override** — `max_retry_attempts`, `grace_period_days`, and
  `final_action` could be overridden but `retry_schedule` could not.
  At runtime the engine merged the override fields into the tenant
  policy at `StartDunning` time.

Two structural problems surfaced during the 2026-05 dogfood pass:

1. **Silent retry-schedule fallback** — when an override set
   `max_retry_attempts=5` against a tenant policy with a 2-entry
   `retry_schedule`, the engine's `processRun` used `idx >=
   len(retryIntervals) { idx = len(retryIntervals) - 1 }` to **reuse
   the last interval** for the extra attempts. Operators saw three
   back-to-back 120h retries they hadn't configured. This is a
   `feedback_no_silent_fallbacks` violation — the engine paper-overed
   a misconfiguration instead of surfacing it at save time.

2. **Partial-field override has no industry precedent.** Multi-platform
   verification on 2026-05-16:

   | Platform | Data model | Per-customer mechanism |
   |---|---|---|
   | **Stripe** | Account-level Smart Retries policy (count + window) | Account-level only; "Automations" for customer-segment policies. No per-individual-customer field override. |
   | **Lago** | Named **Campaigns** (`name, threshold, delay, attempts`). | Assign a different campaign to the customer — whole template swap. |
   | **Orb** | **Dunning Schedules** (templates with step offsets) + **Dunning Rules** (eval order, customer/plan matching) | Customer differentiation via rule matching / exclusion. |
   | **Recurly** | Named **Dunning Campaigns**. | Assign a campaign per Plan or Account. |

   All four converge on **named templates** + **assign-to-customer** (or
   rule-match). Velox's partial-field override is a one-off shape with
   no precedent.

Sources:
- [Stripe — Smart Retries](https://docs.stripe.com/billing/revenue-recovery/smart-retries)
- [Lago — Automatic dunning](https://doc.getlago.com/guide/dunning/automatic-dunning)
- [Orb — Advanced dunning](https://docs.withorb.com/invoicing/advanced-dunning)
- [Recurly — Dunning campaigns](https://docs.recurly.com/recurly-subscriptions/docs/dunning-management)

## Decision

Adopt the converging industry shape: **multi-policy-per-tenant + per-
customer assignment**. Per `feedback_stripe_parity_framing`, where
Stripe is structurally limited (account-level only), adopt the
superset architecture used by Lago / Orb / Recurly (named templates +
assignment).

### Data model

1. **`dunning_policies` becomes multi-per-tenant.** Drop the
   `tenant_id, livemode` UNIQUE constraint. Add `is_default boolean
   NOT NULL DEFAULT false` with a partial UNIQUE index enforcing
   exactly one default per `(tenant_id, livemode)`.

2. **`customers.dunning_policy_id`** — nullable FK to
   `dunning_policies.id`. NULL = use the tenant's default policy.
   Updatable via `PATCH /v1/customers/{id}` (`dunning_policy_id`
   field; empty string = clear).

3. **Drop `customer_dunning_overrides`.** Per-customer differentiation
   now flows through the FK assignment.

### Service-layer resolution

`dunning.Service.GetEffectivePolicyForCustomer(ctx, tenantID,
customerID)` resolves:
1. If `customer.dunning_policy_id` is set → load that policy.
2. Else → tenant's `is_default=true` policy.

Called at `StartDunning` time to bind the policy to a new run.

Once a run exists, `processRun` resolves the policy via `run.policy_id`
(persisted at start) so **mid-flight runs stay on their original
policy** even if the customer's assignment changes. The new
assignment takes effect on the NEXT run.

### Save-time validation (closes the silent-fallback gap)

`UpsertPolicy` now rejects when `max_retry_attempts > len(retry_schedule) + 1`.
The runtime `idx >= len(retryIntervals)` reuse-last-interval branch is
removed; if the engine ever encounters an out-of-bounds index it
fails loudly (a schema-invariant violation, not a runtime fallback).

### Handler surface

- `GET /v1/dunning/policies` — list policies with `assigned_customers` counts.
- `POST /v1/dunning/policies` — create.
- `GET /v1/dunning/policies/{id}` — read one.
- `PATCH /v1/dunning/policies/{id}` — update (cannot flip `is_default`).
- `DELETE /v1/dunning/policies/{id}` — delete; refused for the default policy or any policy with assigned customers.
- `POST /v1/dunning/policies/{id}/set-default` — atomic flip.
- `PATCH /v1/customers/{id}` — `dunning_policy_id` field (empty = clear).

Dropped: `GET/PUT /v1/dunning/policy` (singleton),
`GET/PUT/DELETE /v1/dunning/customers/{id}/override`.

### UI

- New `/dunning-policies` admin page — list, create, edit, set-default,
  delete. Schedule shortage warning rendered inline when the form's
  `max_retry_attempts > retry_schedule.length + 1`.
- Customer detail page — "Dunning policy" card shows the effective
  policy (assigned or default) + "Change" button → `AssignDunningPolicyDialog`
  picking from the policy list.
- `/dunning` page no longer hosts the policy editor (Tabs → single
  Runs view; "Manage policies" link to the admin page).

## Why this design

- **Industry convergence.** All four reference platforms verified
  during 2026-05-16 research use the same named-templates pattern.
  Velox follows that shape rather than inventing a Velox-specific
  one.

- **No silent fallback.** The runtime "reuse last interval" branch is
  gone. Misconfiguration is caught at save time with a clear error
  message naming the missing entries.

- **Migration friendliness.** Existing tenants' singleton policies
  flip to `is_default=true` automatically (migration 0086 backfill).
  No customer data was lost because the pre-launch state has zero
  production customers (`project_state_2026_04`); even if it had,
  the old override semantics are subsumable: any
  `(max_retry_attempts, grace_period_days, final_action)` override
  can be expressed as a new named policy with those values plus the
  tenant's `retry_schedule`, then assigned to the customer.

- **Mid-flight stability.** Runs persist their `policy_id` at start
  time and re-resolve via that id (not via the customer's current
  assignment) for the run's lifetime. An operator reassigning a
  customer doesn't yank the rules out from under in-progress retries.

## Alternatives considered

1. **Extend `CustomerDunningOverride` with `retry_schedule []string`.**
   Pure Velox extension; closes the partial-override gap but ships a
   shape no industry platform uses. Rejected — `feedback_stripe_parity_framing`
   says match the converging industry pattern.

2. **Stripe-style account-level-only.** Drop per-customer
   differentiation entirely. Simplest but doesn't fit operators who
   want differentiated dunning for enterprise vs SMB. Rejected — the
   "we want different retry budgets for high-value customers" need is
   a real B2B SaaS workflow.

3. **Orb-style rules engine.** Customer matching by `plan_ids`,
   `payment_method_types`, `exclusion_lists` with rule priority
   ordering. More flexible than direct assignment but adds a rules
   evaluation layer Velox doesn't need today. Deferred — when a
   design partner asks for "assign by plan tier" we can layer
   rule-based assignment on top of the campaigns model without
   schema churn.

## Consequences

### Positive
- Operator workflow matches Stripe / Lago / Recurly mental model.
- Save-time validation prevents the back-to-back-retry surprise.
- Per-customer flexibility without partial-override confusion.
- Path to Orb-style rule-based assignment open if needed.

### Risks / open items
- **Schema migration is destructive** for `customer_dunning_overrides`
  rows. Velox is pre-launch so no real data lost; down-migration
  re-creates the table empty.
- **In-flight run persistence** assumes `dunning_policies` rows are
  never hard-deleted while a run references them. Application-layer
  `DeletePolicy` enforces this (refuses delete if `assigned_customers >
  0`); the schema-level FK `customers.dunning_policy_id ON DELETE
  SET NULL` is belt-and-suspenders for the customer side. There's no
  FK on `invoice_dunning_runs.policy_id` — if a policy is force-
  deleted via raw SQL while runs reference it, `processRun` errors
  cleanly at lookup time. Acceptable for an operator-controlled
  surface.

## Amendment 2026-05-16 — terminal-action set + pause-semantics fix

Follow-up after operator question "is dunning's pause subscription
option correct industry standard?" — triggered a second multi-platform
research pass on dunning **terminal actions** (the action that fires
after retries exhaust).

### Verified cross-platform terminal actions (2026-05-16)

| Action | Stripe | Lago | Orb | Recurly |
|---|---|---|---|---|
| Cancel subscription | ✓ (default) | ✓ (terminate) | ✗ docs list only retry_payment + email | ✓ (expire) |
| Pause subscription | ✓ (via `pause_collection.behavior`) | ✗ | ✗ | ✗ |
| Mark uncollectible | ✓ ("Mark Invoice as Uncollectible") | — | — | ✓ (analog: "mark invoice failed") |
| Leave open / manual review | ✓ ("Keep active") | ✓ (default) | — | ✓ ("leave overdue") |

Sources:
- [Stripe — Manage failed payments](https://docs.stripe.com/billing/revenue-recovery/smart-retries) — *"Cancel the Subscription (default setting), Mark Invoice as Uncollectible …, or Pause the Subscription until payment is resolved."*
- [Lago — automatic-termination feature](https://getlago.canny.io/feature-requests/p/trigger-automatic-subscription-termination-based-on-dunning-campaigns)
- [Orb — advanced dunning](https://docs.withorb.com/invoicing/advanced-dunning) (no state-change actions documented)
- [Recurly — dunning management](https://docs.recurly.com/recurly-subscriptions/docs/dunning-management)

### Decision

Velox supports all four actions; semantics aligned with Stripe.

*(Superseded by ADR-112 — these are no longer four alternatives on one column
but two independent choices. The behaviours described are unchanged.)*

| Velox enum (2026-05-16 amendment; column split by ADR-112) | Behavior |
|---|---|
| `manual_review` | Run lands state=escalated. No sub/invoice mutation. Operator handles via dashboard. Maps to Stripe "Keep active." |
| `pause` | **Calls `subscription.Service.PauseCollection(behavior=keep_as_draft)`** — not the pre-amendment hard `PauseAtomic`. Cycle keeps drafting invoices; charging + dunning paused until operator resumes. Matches Stripe's `pause_collection.behavior=keep_as_draft`. The hard-pause path silently skipped invoice generation for the affected periods (per ADR-035 analysis) — non-Stripe and destructive. |
| `mark_uncollectible` | Calls `invoice.Service.MarkUncollectible` — flips the unpaid invoice to status='uncollectible' (new status, migration 0089). Stripe-standard semantics: receivable closed but invoice stays in financial reporting. Distinct from voided. |
| `cancel_subscription` | Calls `subscription.Service.Cancel`. Stripe-default terminal action; supported by 3 of 4 reference platforms. Previously missing — operator workflow required a manual cancel step after escalation. |

Old `write_off_later` enum value mapped to `mark_uncollectible` during
the migration (semantics-identical, industry-standard spelling).

### Implementation slices

1. **Migration 0088** — drop CHECK on `dunning_policies.final_action`,
   re-add accepting the four new values, backfill `write_off_later → mark_uncollectible`.
2. **Migration 0089** — add `uncollectible` to the `invoices_status_check` CHECK constraint.
3. Domain — rename `DunningActionWriteOff → DunningActionMarkUncollectible`,
   add `DunningActionCancelSubscription`. Add `InvoiceUncollectible` to invoice status enum.
4. Service interfaces — `SubscriptionPauser.Pause → PauseCollection`,
   new `SubscriptionCanceler.Cancel`, new `InvoiceUncollectibleMarker.MarkUncollectible`.
5. `dunning.Service.exhaustRun` switch covers all four cases.
6. `invoice.Service.MarkUncollectible` — flips status with the same
   transition guards as `Void` (paid/voided/already-uncollectible refused).
7. Router adapters wire `SetSubscriptionPauser`, `SetSubscriptionCanceler`,
   `SetInvoiceUncollectibleMarker`.
8. SPA `DunningPolicies.tsx` dropdown shows the four options with
   Stripe-aligned labels (pause label explicitly says "keep drafting").
9. Invoice activity timeline's `describeDunningEvent` recognizes the
   three state-change reasons.

### Why each call

- **`pause` → PauseCollection**: matches Stripe semantics. The hard-pause
  path silently skipped cycle billing for the affected periods, leaving
  no record of customer activity during the paused window — destructive.
  PauseCollection(keep_as_draft) keeps the cycle running so the operator
  has an audit trail.

- **Add `cancel_subscription`**: 3 of 4 platforms support it; Stripe
  defaults to it. Velox required a manual operator step post-escalation
  before this amendment — non-standard.

- **Rename `write_off_later → mark_uncollectible`**: same intent, but
  `mark_uncollectible` is the Stripe-standard term. `write_off_later`
  is a Velox-only spelling with no industry analog.

- **Add `InvoiceStatusUncollectible`**: enables true Stripe-parity for
  the mark-uncollectible action. Pre-amendment the status didn't exist
  and the option couldn't be wired correctly (would have had to use Void
  as an approximation, which is destructive in different ways).

## References

- ADR-029: fully disjoint test-clock flows
- ADR-030: simulated time everywhere on clock-pinned entities
- ADR-035: per-fact simulated-time anchoring
- Migration 0086: `dunning_policies_campaigns_model` (initial decision)
- Migration 0088: `dunning_policies_final_action_enum` (amendment — 4-action set)
- Migration 0089: `invoices_status_uncollectible` (amendment — new invoice status)
- Memory: `feedback_stripe_parity_framing`, `feedback_verify_stripe_parity_claims`,
  `feedback_no_silent_fallbacks`, `feedback_reference_platforms`,
  `feedback_enum_check_constraint_audit`

## Amendment (2026-07-05) — initial default + no-policy is a deliberate skip

The multi-policy model above created a **no-default trap**: `upsertPolicyTx`
inserted every new policy with `is_default=false` and only `SetDefaultPolicy`
could designate one, so the normal state after a recipe instantiation (or a
tenant's first manual policy) was *"a policy exists, but `is_default=false`"* →
`GetDefaultPolicy` → `ErrNotFound` → `GetEffectivePolicyForCustomer` errored →
the no-payment enrollment sweep reported `had_errors` and poisoned the billing
catchup for an unpaid invoice. Surfaced live 2026-07-05 (a recipe tenant's
`"AI default retry"` policy was `is_default=false`).

Two orthogonal fixes (validated by a divergent design panel):

1. **Initial-default-at-first-create.** The FIRST policy per `(tenant, livemode)`
   is born `is_default=true`, computed in-statement as
   `NOT EXISTS(SELECT 1 FROM dunning_policies WHERE is_default)` (RLS-scoped to
   the current tenant+livemode). Both `UpsertPolicy` (manual) and `UpsertPolicyTx`
   (recipe) inherit it. Subsequent policies stay `is_default=false`; the operator
   re-points via `SetDefaultPolicy` (still the sole atomic demote-all/promote-one
   path). `is_default` continues to mean exactly what it says — the row is
   genuinely flipped, so `GetDefaultPolicy`/UI/analytics all agree (rejected
   *resolve-to-sole*, a read-time patch that leaves `is_default` lying in the DB
   and introduces a 1→2-policy silent-stop cliff). The partial unique index
   `idx_dunning_policies_one_default_per_tenant` is the loud backstop for a
   concurrent-first-insert race — no savepoint-retry (creates are serial; add it
   under a named concurrency need). This generalizes migration 0086's own
   one-time backfill (`UPDATE … SET is_default=true`) to every future tenant.

2. **No effective policy = a deliberate skip, not an error.** `StartDunning` maps
   *only* `ErrNotFound` from `GetEffectivePolicyForCustomer` to
   `errs.InvalidState("dunning not configured")` (with a WARN) — the same
   deliberate-skip class as a *disabled* policy, which the enrollment adapter
   already swallows. So a genuinely zero-policy tenant (a plain bootstrap — which
   deliberately seeds **no** dunning policy, to avoid shadowing recipe policies)
   never poisons the money-path catchup: an unconfigured optional feature must not
   break core billing (the SMTP-unset precedent). A real infra error still
   propagates (fail loud — no silent fallback). Companion: `startDunningWithRetry`
   short-circuits `ErrInvalidState` so disabled/no-policy tenants don't burn the
   retry budget or emit the misleading "operator must start manually" ERROR.

Resolution precedence (per-customer `dunning_policy_id` → tenant `is_default`)
is unchanged. Memory: `feedback_adversarial_panel_before_build`,
`feedback_settings_aspirational_runtime_enforcement`, `feedback_no_silent_fallbacks`,
`feedback_no_belt_and_suspenders`, `feedback_longterm_fixes`.
