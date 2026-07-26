# ADR-085: A recipe is an idempotent, additive provisioning event — one verb, no uninstall

**Date:** 2026-07-08
**Status:** Accepted

## Context

ADR-083 (2026-07-07) gated recipe **adoption** on conformance: `Instantiate` adopted an existing plan by `code` or meter by `key`, and refused (409) if the adopted object's money-affecting config diverged from what the recipe declared. That closed the silent-substitution bug, but the design panel that produced it kept spawning containment machinery on top of the underlying shape — plan conformance diff, provenance stamps (deferred), a `Force` flag reserved-but-unwired, a `DELETE /v1/recipes/instances/{id}` uninstall that only dropped the index row while the entities it named stayed live, and `seed_sample_data` scaffolding (`RecipeSampleData`/`SampleCustomer`/`SampleSubscription`, `CreatedObjects.CustomerIDs`/`SubscriptionIDs`) that was fully parsed and validated but had zero writers — nobody ever called it. Per `feedback_complexity_accretion_smell`: when a design keeps growing containment around a self-made bug class (adopt-by-key → conformance-diff → deferred provenance → force-flag escape hatch), the shape itself is wrong, not under-defended.

The self-made bug class was **adopt-by-natural-key** in the first place. A recipe adopting a plan by `code` requires proving the adopted plan matches the recipe's intent — an open-ended, ever-growing surface (ADR-083 covered 5 fields and explicitly flagged `tax_code` as the next debatable one). The fix that dissolves the class instead of fencing it: **a recipe never adopts a plan.** Every apply mints a fresh plan. There is no "does this existing plan match?" question because there is no existing plan in the loop.

Once that's the base case, the rest of the accreted machinery turns out to be answering a question nobody actually asks:

- **`Uninstall`** presumes a recipe *owns* what it created, the way `helm uninstall` owns a release's rendered manifests. It doesn't — the plan the recipe generated can carry live subscriptions the moment a customer signs up, and the rating rules/meter it adopted are shared reference data another recipe or the operator may also be using. `DELETE /v1/recipes/instances/{id}` (the design's original contract) only ever dropped the `recipe_instances` index row — "no cascade" was called out as intentional in `docs/design-recipes.md` from the start. An operation that can't retract anything isn't an uninstall; it's a badge deletion that makes the audit trail lie about what was applied.
- **`seed_sample_data`** answers "give me fake data to click around" — a demo-seeding need, not a provisioning one (`feedback_no_seed_data_shortcut`: no seed-demo-data buttons; empty + educate, or a separate demo tenant). It shipped fully parsed and validated and was never wired to a writer — the honest state of an unbuilt feature, not a working one.
- **`Force`** existed to blow away and redo an install. Once install never collides (plans are never adopted) there's nothing to force through.

The actual repeat use case operators have is different from all of the above: **re-run the same recipe key against a template that's moved on** — new model pricing, a new default plan shape — without disturbing the plan a live customer is already on. That's an *update*, and Velox doesn't build it here (see Deferred) — but naming it correctly is what falsifies "uninstall" as the missing piece. Nobody asks to un-provision a recipe; they ask to re-provision one, additively, next to what's already there.

## Decision

A recipe is an **idempotent, additive provisioning EVENT**, not an owned resource. One verb: **apply** — `POST /v1/recipes/{key}/instantiate`.

`recipe.Service.Instantiate` (`internal/recipe/service.go`) runs, in one `db.BeginTx(ctx, postgres.TxTenant, tenantID)`:

1. **Idempotency gate first.** `store.GetByKeyTx` checks the badge (`recipe_instances`, keyed `UNIQUE(tenant_id, livemode, recipe_key)` since migration `0157`; `0055` shipped it mode-blind — see the 2026-07-26 amendment below). If it exists, return it — no further work, no writes attempted.
2. **Adopt-or-create the shared catalog.** Rating rules adopt an existing `rule_key` at its current version, never republish (see Money safety #2). Meters adopt an existing `key` only if `aggregation` matches, else refuse loud (see Money safety #3).
3. **Generate one fresh, born-unique plan.** `freePlanCode` (`internal/recipe/service.go`) takes the recipe's desired code and, if it's taken, walks `<code>_2`, `<code>_3`, … until it finds a free one — the *incumbent* is never touched, renamed, or read for comparison. Wired to the adopted/created meter; base fee amount and `base_bill_timing` come from the operator's instantiate-time params (`RecipePlan.BaseBillTiming`, verbatim, no second-guessing); currency is inherited from the adopted rating rules, not from the plan's own override.
4. **Existence-checked create** for the (optional) dunning policy and webhook endpoint — `UpsertPolicyTx` / `CreateEndpointTx`, no adoption question because these aren't natural-keyed shared state the way a plan code is.
5. **Write the badge.** `store.CreateTx` persists `recipe_version` + `created_object_ids` (`domain.CreatedObjects`) — the permanent record of what this apply touched.

### Idempotency

The badge is the gate, checked before any write. Re-applying an already-installed recipe is a **no-op that returns the existing instance** — same `id`, same `created_objects`, same HTTP `201`. Never a 409, never a duplicate plan.

**Amendment (2026-07-26, m0157): the badge is scoped per livemode.** `0055` shipped the badge mode-blind while everything a recipe provisions is mode-partitioned (0020) — so a test-mode install's badge gated the live-mode install of the same recipe, which "no-op'd" into a Live dashboard showing *Installed* with zero live pricing objects. The badge now carries `livemode` (0020 treatment: mode-scoped unique, RLS predicate, session-stamped insert trigger), so "applied once" means *once per mode*, matching the objects the badge records. The class lesson: a mode-blind tenant table is only safe when its lookups ride FK-scoped mode-aware parents; the badge is addressed by a **natural key** (`recipe_key`), the one shape that argument doesn't cover — `TestRLSIsolation_AllModeAwareTablesHaveLivemodePredicate` now derives the mode-aware set from the schema so the next such table can't ship silently.

### No uninstall, no dismiss

There is no `DELETE` route on `/v1/recipes/instances/{key}` and no store method to drop one — `Store` (`internal/recipe/store.go`) exposes only `GetByKey`, `List`, `GetByKeyTx`, `CreateTx`. A recipe's objects are owned by the operator the instant they exist: plans carry live subscriptions under `ON DELETE RESTRICT` with no hard-delete path, and the catalog (rating rules, the `tokens` meter) is shared reference data another install may also depend on. There is nothing an uninstall could safely retract. The badge is a permanent truthful record of what was applied and when. To retire the generated plan, the operator archives it — the existing plan-domain verb, no new mechanism needed.

### Meter guard

Adopting an existing `tokens` meter is gated on **aggregation matching** — the sole billing-consulted meter field (`engine.go`'s `deferredBucket`/`mapMeterAggregation` roll up usage off it; `name`/`unit` are display labels). On a mismatch, `Instantiate` refuses loud: `errs.AlreadyExists("meter", …)`, naming the field (`"usage aggregation: recipe wants sum, existing is max"`), and the whole transaction — including any rating rules already adopted/created earlier in the same call — rolls back. There is deliberately **no live-subscription clause**: adoption only *reads* the meter to wire it onto the fresh plan and append disjoint pricing bindings; it never mutates the meter, so aggregation-match is a complete safety condition on its own. That completeness is also what lets a second AI recipe (e.g. a future `openai_style` re-apply, or a hand-authored plan) adopt the shared ADR-044 `tokens` meter post-go-live without a provenance system to arbitrate "whose meter is it."

### Never republish an existing rule key

Usage bills the **latest active version of a rule key as-of period start** (`resolveRatedRule` → `GetRuleByKeyAsOf`, ADR-070) — that's the pinned-at-period-open resolution every consumer (cycle close, preview, threshold scan) shares. If `Instantiate` published a new version of a key that already exists, every live subscription resolving that key would reprice at its *next* period open, silently, from an operator action that only asked to install a recipe. So rating rules are **adopted at their current version, never republished** — this is the load-bearing no-reprice invariant the whole apply design stands on. The direct consequence: **recipe apply is additive, net-new only.** It can never change an existing price. Changing a price is an explicit, separate pricing action (`PATCH`/publish through `pricing.Service`), out of recipe scope entirely.

## Money safety

1. **Fresh plan is always meter-wired.** Because a plan is never adopted — only ever generated fresh, in the same transaction as the meter it binds to — silent-$0 billing from an under-wired plan is structurally impossible on this path.
2. **Adopt-not-republish + plans immutable-once-live** (the existing `UpdatePlan` guard on plans carrying active subscriptions) means no re-apply of a recipe can reprice a live subscription. Rule versions don't move under a live sub's feet, and a live sub's own plan can't be rewritten by a recipe re-run.
3. **Meter adoption is aggregation-only, and refuses loud on the one ambiguous case.** The single billing-consulted field is checked; a mismatch stops the whole apply rather than mis-rolling-up usage under a silently wrong aggregation.
4. **Currency is inherited from the adopted/created rating rules**, not independently chosen for the plan — a recipe can't mint a plan priced in a currency its own rules don't bill in.

**Narrow honest limit:** this does **not** close the general "a hand-authored, meter-less plan bills usage at $0 via a server-log WARN nobody tails" path. That's a real gap, but it's a boundary-detector problem across the whole pricing surface, not something a recipe's own money-safety story can claim — it stays a deferred, separately-tracked item (see `feedback_no_silent_fallbacks`).

## Why update-not-uninstall

The real repeat action operators take against a recipe isn't "take this back" — it's "re-run this against a newer template." Verified against how adjacent tool classes handle exactly this shape:

- **Billing platforms version prices by create-new-and-archive-old, never mutate a live price in place** — Stripe Prices are immutable once created (you archive the old one and create a new one to change an amount); Orb's pricing model is the same append-only-version shape. That's precisely the constraint ADR-070's adopt-not-republish rule encodes for Velox's own rating rules.
- **Generator/scaffolding tools hand off their output and have no uninstall** — `create-react-app`, `vercel deploy`, Backstage software templates all generate an artifact and step away; there's no corresponding teardown command, because the generator doesn't own what it produced once it exists. That's the exact shape of a recipe-generated plan.
- **Infra-as-code tools that DO expose a teardown (Helm `uninstall`, Terraform `destroy`) do so because they continuously own and reconcile a set of managed resources across every subsequent change.** A recipe doesn't reconcile anything after the one apply call — it's a single generative event, closer to `helm template | kubectl apply` than to a Helm release. The tools in that same continuous-ownership category instead expose **inspect + upgrade** (`helm get`, `helm upgrade`), not delete-and-redo.

So "install" is the one real event a recipe performs. "Update" — re-applying additively against a newer template version — is the real repeat use case, deferred below because it needs execution semantics this ADR doesn't build yet. "Uninstall" isn't a real operation at all: nothing in the object graph a recipe touches is safe to retract, and the code's own migration-era comment already said as much (`docs/design-recipes.md`: "no cascade... this is intentional").

## Deferred

Each item below is cut for a **named** reason, with the trigger that reopens it:

- **Additive update-execution, including conditional new-plan-on-packaging-change.** Re-applying a recipe key today is a pure no-op once the badge exists — there's no "the template moved, adopt what's still compatible and mint what's new" path. **Trigger:** a second version of a built-in recipe template ships *and* a tenant already holds an instance of the prior version.
- **"Newer version available" nudge.** Surfacing "anthropic_style v2 is out" on the recipe card. **Trigger:** update-execution above ships first — the nudge is pointless without a corresponding action.
- **Re-apply preview-diff.** Showing what an update-execution *would* change before committing, mirroring the existing install-time `Preview`. **Trigger:** same as update-execution.
- **Push-reprice-to-live-subs, as an explicit opt-in migration.** The only correct way to reprice existing customers is a deliberate, operator-initiated migration flow (Orb/Metronome-style price-migration primitives) — **never** a side effect of a recipe re-run. Deferred as its own design; recipes must never grow an implicit version of it.
- **Object cleanup — archiving recipe-created meters/rules.** Meters and rating rules have no archive path today at all (only plans do), so there's no safe way to retract them even if a recipe wanted to offer it. **Trigger:** the first operator who needs to decommission a recipe-created meter, forcing the archive-meter design regardless of recipes.
- **Real demo seeding.** The removed `seed_sample_data` scaffolding solved a demo-data need with provisioning-event machinery. Per `feedback_no_seed_data_shortcut`, the right shape is empty-and-educate or a dedicated demo tenant, not a flag on the money-provisioning call. **Trigger:** none named yet — revisit if onboarding friction data shows first-run emptiness is a real drop-off point.
- **A general "unbilled usage is loud" detector.** Closing the hand-authored-meter-less-plan $0 path named in Money Safety's honest limit. **Trigger:** the first observed instance of silent $0 usage billing outside the recipe path.

## Alternatives considered

- **The conformance gate (ADR-083, #407).** Shipped one day prior; refuse-on-divergence for adopted plans/meters. Worked, but false-alarmed on legitimate cases (an operator's own base-fee edit on a recipe-created plan, or a same-code plan from an unrelated source that happened to match closely but not exactly) and needed a provenance stamp to even tell "operator edited their own recipe plan" apart from "foreign collision" — a distinction the gate itself admitted it couldn't make. Superseded here: once a plan is never adopted, there is no divergence to detect.
- **Reuse-and-warn (ADR-084, #408 — never merged).** A softer alternative floated between ADR-083 and this one: adopt a same-code plan if it's "close enough," warn instead of refuse. Rejected before merge — it dropped the meter aggregation check entirely and reopened the exact silent-under-billing hole ADR-083 had just closed. Referenced here only because it's a real, considered, rejected design; it never shipped and `docs/adr/084-*.md` does not exist on this branch.
- **A "dismiss" verb that deletes the badge but leaves the objects.** Solves the UI's "make this card go away" want without touching the entities. Rejected: the badge is the one place recording "this recipe was applied, here's what it created" — deleting it while the plan/meter/rules live on makes the audit trail actively false. An operator or future support engineer reading `recipe_instances` would see a smaller, wrong picture of the tenant's history.
- **`transfer_lookup_key`-style stable codes for the generated plan.** Considered as a way to give a recipe's plan a durable, human-legible slug across re-applies (mirroring Stripe's `lookup_key` transfer between Prices). Rejected as unwarranted machinery: `plan_code` has no functional reader in Velox — subscriptions bind by immutable plan `id`, not code — so building transfer semantics for a field nothing reads is cosmetic complexity with no payoff.

## Consequences

### Positive
- The plan-conformance / provenance / force-flag containment chain from ADR-083 is gone outright rather than grown further — the underlying adopt-by-key shape that produced the bug class no longer exists on the plan path.
- Re-applying a recipe is safe to retry blindly (network retry, double-click, at-least-once webhook-triggered automation) — it converges on the same instance every time.
- The badge (`recipe_instances`) stays a permanently truthful record: everything it names is guaranteed to still exist (nothing it created is ever deleted by recipe machinery), so a future support/audit read of the table is never lying about tenant history.
- Dead code shipped-but-never-wired (`seed_sample_data`, `Force`, `DELETE /instances/{id}`) is removed rather than left as an attractive nuisance for the next contributor to assume works.

### Risks / open items
- **No update path yet.** A tenant on an older recipe template has no in-product way to pick up new model pricing short of a manual pricing edit — named as the first deferred item, with its trigger stated above.
- **Plan-code proliferation.** A tenant that repeatedly hand-creates plans colliding with a recipe's default code (or re-runs recipe families that share a default code) accumulates `_2`, `_3`, … suffixed plans over time. Acceptable: plan code is a display slug, not a functional identifier, and the alternative (renaming or mutating the incumbent) is strictly worse.
- **`internal/domain/recipe.go`'s `RecipeSampleData`/`SampleCustomer`/`SampleSubscription` types and `CreatedObjects.CustomerIDs`/`SubscriptionIDs` fields, and the YAML parsing/templating that populates them, are dead code left in place** — parsed and validated at recipe load, never written by `Instantiate`. Tracked for removal alongside the next recipe-package cleanup pass; not a money-safety issue (nothing reads or writes through them), but it's a doc-code-truth gap worth closing rather than leaving as a trap for the next reader who assumes a populated struct field means a working feature.

## References
- ADR-083 (recipe adoption conformance gate — superseded by this ADR)
- ADR-084 (#408, reuse-and-warn — never merged; referenced, not superseded, since it never shipped)
- ADR-070 (rating-rule resolve-as-of-period-start — the no-reprice invariant this design depends on)
- ADR-044 (canonical AI token-metering model — the shared `tokens` meter the aggregation guard protects)
- ADR-031 (per-plan `base_bill_timing` — how a recipe's generated plan gets its timing)
- `internal/recipe/service.go` (`Instantiate`, `freePlanCode`), `internal/recipe/conformance.go` (`meterConformanceDiff`), `internal/recipe/store.go`, `internal/recipe/handler.go`, `internal/domain/recipe.go`
- Migration `0055_recipe_instances` (badge schema — unchanged by this ADR)
- Memory: `feedback_complexity_accretion_smell`, `feedback_no_silent_fallbacks`, `feedback_no_seed_data_shortcut`, `feedback_longterm_fixes`
- `docs/design-recipes.md` (amended alongside this ADR)
