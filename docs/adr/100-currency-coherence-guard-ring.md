# ADR-100: Currency coherence is enforced at every write — a guard ring, not engine reconciliation

**Date:** 2026-07-26
**Status:** Accepted

## Context

Velox amounts are unit-less minor-unit cents; every currency LABEL is attached downstream of the math. The billing engine resolves ONE invoice currency (billing profile > first item's plan > tenant settings > USD) and stamps it over rule-computed cents **without conversion or comparison** — its own comment called mismatches "a pricing config error, not a billing problem to solve here." A full site-set enumeration (money-path playbook, 2026-07-26; six parallel readers over every currency writer/reader) found that config error was *writable from eight directions*, three of them wrong-money paths that reach a Stripe PaymentIntent:

1. **Rule republish** could change a key's currency while plans billed it (the ADR-070 guard checked only customer overrides) — and the dashboard made it likely: the rule-edit form stamped the tenant's *current* default currency with no picker, so a Settings currency switch turned any later rate edit into an unintended currency change.
2. **Profile-vs-plan divergence by ordering**: ADR-067 blocked changing a profile's currency while subs billed, but setting the profile FIRST and creating the sub SECOND passed — subscription create validated interval, timing, archival, and meter overlap, but not currency. The engine then relabels plan-computed cents under the profile currency unconditionally ($100 invoiced as €100). Drafts were also invisible to the ADR-067 checker.
3. **Zero-decimal currencies** (JPY/KRW/VND/CLP) passed validation while every renderer divides by 100 and Stripe reads their amounts as WHOLE units — a latent 100× overcharge.

Plus: meter *bindings* (pricing-rule rows and the default binding) accepted any-currency rules with no republish or plan-create involved; plan **update** could attach divergent meters; mixed-currency item sets passed every subscription guard; a cross-currency swap subtracted old-plan cents from new-plan cents raw; the credit ledger is a currency-blind per-customer pot draining 1:1 across denominations; and the engine's precedence comments were born false at all three sites (code is profile > plan; the settings branch is dead because plans always carry currency).

An adversarial panel (three lenses: legitimate-flow breakage, completeness, blast radius) reshaped the first design with seven blockers — the binding writers, the plan-update bypass, the single-item swap hole, the FE write-path stamps, an archived-plan repair deadlock, key-resolution semantics, and honest repositioning of the settle-time check.

## Decision

**Refuse divergence loudly at every write point; change nothing in the engine.** The engine's single-currency stamping is *correct* once divergence is unwritable — reconciliation/conversion machinery would be complexity without a customer. The ring:

- **G1 — rule republish** (`guardRuleCurrencyChange`): a currency-changing republish is refused while the key is bound — through *either* edge (meter pricing rules or a meter's default binding) — to any **in-scope plan** in a different currency. In-scope = non-archived plans plus archived plans still carrying non-terminal subs (those keep billing); archived sub-less plans are excluded so the repair flow (archive the wrong-currency plan, republish the key) stays open. The ADR-070 overrides clause stays.
- **G2 — plan create AND update-with-meters** (`guardPlanMeterCurrencies`): a plan's currency must equal the **resolved** currency of every rule key its wired meters bill. Resolution mirrors the engine (`resolveRatedRule`): latest ACTIVE version by key — never the binding row's pinned version, which can lag a legal unbound republish. Keys whose active versions mix currencies are refused outright.
- **G2b — binding writes** (`guardMeterBinding`): `UpsertMeterPricingRule` and `UpdateMeter`'s default-binding rebind refuse a rule whose key resolves to a different currency than the meter's other bound keys or any in-scope plan wiring the meter. **A meter's prices share one currency** — this is the pre-launch stance (see Deferred).
- **G3 — customer currency pin** (`rejectCurrencyMismatch`, Stripe-parity): at sub create, add-item, both swap paths, and Activate, the plan's currency must match (i) the sub's other items, (ii) the swap's **outgoing plan** (proration is raw cents), (iii) the profile currency if set, (iv) the customer's other non-terminal subs including `pending_plan_id`. Activate closes the pre-existing-divergent-draft window; the ADR-067 profile checker now includes drafts (statuses parameter — the archive guard keeps its narrower set so "still bill" copy stays true).
- **G4 — recipe path**: `CreateRatingRuleTx` gains normalization + allowlist + the republish clauses (closing the archived-key edge); recipe apply probes an **adopted** meter for cross-currency rules (`AdoptedMeterCurrencyConflict`) — two recipes with disjoint keys must not stack currencies on the shared `tokens` meter. `UpsertMeterPricingRuleTx` is deliberately unguarded: inside the coordinator tx the bound rules were created/adopted in the same call and committed-state reads can't see them; recipe-level coherence covers it.
- **G5/G6 — entry validation**: billing-profile currency is allowlist-validated (was format-only, and it's the highest-precedence header source); manual invoice currency is allowlist-validated when non-empty (the empty→USD default is a working API contract and stays).
- **G7 — zero-decimal cut**: JPY/KRW/VND/CLP removed from the allowlist with a distinct "no minor unit — charging would be off by 100×" refusal. FE currency list trimmed in the same change (plus TRY, which the backend never accepted — a pre-existing lying dropdown).
- **G8 — settle tripwire**: the webhook settle path — the one entry point carrying Stripe's independently-reported charge currency — compares it case-folded against the invoice header and raises a `payment.currency_mismatch` anomaly (report-only, finance-readable attention copy). Positioned honestly: Velox creates the PI *from* the header, so this catches out-of-band/mutation cases only, not ring escapes.
- **FE write-path truth**: the rule-edit dialog preserves an existing key's currency instead of stamping the tenant default; plan create inherits currency from its wired meters' rules when resolvable.
- **G9 — prose**: the three engine precedence comments rewritten to the real order; the `"usd"` fallback literals uppercased (canonical-case invariant; pre-insert consumers see the in-memory value).

## Consequences

- A tenant selling in two currencies needs **separate meters/rule keys per currency and separate customers per currency** — coherent-by-construction rather than ambiguous. This is a real product limitation, accepted pre-launch (see Deferred: multi-currency catalog).
- Wrong-currency installs are repairable: archive the sub-less plan → republish the key → recreate the plan.
- All guards fail CLOSED for single writers. Concurrent writer pairs (profile-save racing sub-create, republish racing plan-create) are check-then-act TOCTOU windows — deferred with the serialization design named (customer-scoped and catalog-scoped `pg_advisory_xact_lock`), trigger: first multi-operator tenant.

## Deferred (register entries in velox-ops)

- **Credit-ledger currency column** — the ring narrows but does NOT close cross-currency draining: a canceled USD sub's unpaid invoices + a new EUR sub passes clause (iv) (non-terminal only), and manual invoices aren't customer-currency-pinned. Trigger: first multi-currency tenant need, or the ledger project.
- **Multi-currency price catalogs** (Stripe `currency_options` parity — Velox is BEHIND here, knowingly). Trigger: first DP selling one product in two currencies.
- **Zero-decimal currency support** (amount semantics per currency at Stripe boundary + renderers). Trigger: first JPY/KRW-pricing DP.
- **TOCTOU serialization locks** as above.

## References

- Guards: `internal/pricing/currency_coherence.go`, `internal/subscription/service.go` (`rejectCurrencyMismatch`), `internal/customer/service.go`, `internal/payment/stripe.go` (tripwire)
- Tests: `TestCurrencyRing_*` (pricing), `TestCurrencyPin` (subscription), `TestService_Instantiate_RefusesCrossCurrencyMeterAdoption` (recipe)
- Amends: ADR-067 (profile guard: drafts included, one-directionality closed), ADR-070 (republish guard: plans clause added), ADR-068 (settle truth-check: currency tripwire sibling)
