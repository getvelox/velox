# ADR-108: Parked invoices are resolved by provider search — adopt what you can name, never settle absence

**Status:** Accepted
**Date:** 2026-08-02
**Builds on:** ADR-105 (seq-seeded idempotency key), ADR-107 (park, never settle
an unknown outcome), migration 0167 (provider observation columns).
**Amends:** ADR-107's "cannot be reconciled automatically" premise — see its
2026-08-02 amendment. The conclusion survives on a corrected ground; this ADR
is the constructive half.

## Context

ADR-107 parks an invoice whose charge attempt returned an ambiguous outcome with
no PaymentIntent id: `payment_status='unknown'`, empty PI id, admitted by no
charge path, exited only by an operator write-off. That was designed as "stuck
and loud beats silently double-charged" — and its premise was that an unnamed
PaymentIntent is unreachable.

The premise is false. Every engine-minted PaymentIntent has always carried
`velox_invoice_id` in its metadata (stripe.go, stamped at create), and Stripe's
PaymentIntent Search API can find PIs by metadata. Verified 2026-08-02 against
the vendored SDK (v82.5.1: `sc.V1PaymentIntents.Search`) and Stripe's docs:
search works in test mode and sandboxes (separate 20 reads/sec limits per
mode), minimum API version 2020-08-27, **not available to businesses in India**,
and — load-bearing — *"data is searchable in under 1 minute… propagation of new
or updated data could be delayed during an outage."*

So the question was never "can we find it" but "what may we do with each
possible answer". This design went through the full money-path discipline: a
three-sweep site-set enumeration, a four-judge design panel (which scored the
parked ADR-106 ledger 4–0 below the shipped design), and a 27-scenario
adversarial attack round against two candidate designs. What follows is the
shape that survived.

## Decision

A third reconciler sweep over exactly the parked population searches Stripe by
`metadata['velox_invoice_id']` and acts on **positive evidence only**:

| Search finds | Action |
|---|---|
| Any **succeeded** PI | Settle paid with it (newest if several) through the ordinary settle primitive. Additional succeeded PIs are left to the webhook path, whose already-paid-different-PI branch escalates the duplicate-charge anomaly (ADR-068) exactly as today. |
| Else any **live** PI (`processing`/`requires_action`/`requires_confirmation`/`requires_capture`) | **CAS-adopt** the newest live PI: a conditional store write that stamps the id and moves `unknown→processing` only while the row still matches the full parked shape. Zero rows affected means another path won the race — do nothing. The invoice joins the ordinary reconcilable population and resolves through the existing sweeps/webhooks. |
| Only terminal-failed/canceled PIs, or **nothing**, or a search **error** | **Record the observation (0167 columns) and write no money outcome.** The invoice stays parked: gauge counts it, banner names the write-off, mark-uncollectible remains the exit. |

**Absence from search results never writes a money outcome.** That single
sentence is the design. Two attack-round BREAKS forced it:

- The outage that *mints* a parked row is the same weather that delays search
  indexing — the two probabilities are correlated, so "searched twice, found
  nothing" is weakest exactly when it matters. An empty result can never prove
  no PI exists.
- Metadata carries the *invoice* id, not the *attempt* id. A stale declined PI
  from attempt N−1 is long-indexed while the newest PI is the one lag hides —
  so "only terminal PIs found" cannot prove the *current* attempt is terminal
  either. Settling failed off either signal is the ADR-107 give-up write
  re-armed behind an unreliable negative; both arms were deleted from the
  design.

The found-PI arms are safe because they act on a **named** PI — which is what
ADR-107 always treated as the resolution (its ordinary path is the webhook
naming the PI). Search is simply a second, pull-shaped way to learn the name.

### Guards that ship with it (each from a verified attack)

1. **The adopt is a CAS on the parked shape**, not a plain `UpdatePayment`:
   `… SET stripe_payment_intent_id=$1, payment_status='processing',
   charge_attempt_seq = charge_attempt_seq + 1 … WHERE payment_status='unknown'
   AND COALESCE(stripe_payment_intent_id,'')='' AND status='finalized'`.
   The webhook racing the sweep is the *common* case (both fire when Stripe
   recovers); an unconditional stamp would regress a just-settled invoice to
   in-flight. The bump rides the stamp per the ADR-105 CI gate: adopting a PI
   records a named attempt.
2. **`livemode` is in the list predicate**, mirroring `listInflightPayments`
   (the #13 bug class): a mode-mismatched search is empty with probability 1,
   and this design deliberately makes empty results inert — but a mode bleed
   would still waste the search budget and stall rotation.
3. **The sample cool-off lives in the list SQL**
   (`provider_synced_at IS NULL OR provider_synced_at < now() - interval '1 hour'`),
   so rotation, rate-limiting, and re-search pacing are one mechanism. Stamping
   every search result (found or not) is then correct by construction.
4. **Search-refused is observable and self-limiting**: a per-class error counter
   (`velox_parked_search_errors_total`), WARN per occurrence, and an
   invalid-request class (the not-offered/India shape — a request-shape error
   that will not heal) disables the sweep for that tenant+mode for the process
   lifetime with a single CRITICAL. Those tenants keep exactly today's
   behavior: parked + gauge + write-off exit.
5. **Age gate:** only rows parked ≥ 1 hour enter the sweep — 60× the documented
   normal indexing lag, so the common webhook race is always won by the webhook
   and early empty searches (which we would ignore anyway) are mostly avoided.

### Honesty sweep (same PR, docs-don't-lie)

The parked copy said "it will not resolve on its own" — unconditionally true
under ADR-107, conditionally false now. Banner and gate copy become bounded:
Velox searches the provider for the attempt; if it can be found the invoice
resolves automatically; if it cannot, it will not resolve on its own and the
write-off remains the exit. FLOW I4c gains the search boxes; the park-site
CRITICAL log names the sweep instead of implying permanent manual-only
resolution.

## What this deliberately does not do

- **No settle-failed, ever, from search results.** The one way to move a parked
  invoice to `failed` remains a webhook naming the PI (or adoption followed by
  the ordinary paths). The residual — a PI that truly never existed — still
  ends in the operator write-off. That residual is smaller than it looks: it
  requires the response lost AND no PI created AND the operator to care before
  ever re-collecting, and the write-off has been the sanctioned exit since
  ADR-107 shipped.
- **No replay.** The no-bump/replay experiment is refuted (ADR-106, ADR-107
  amendments): reconstructed keys cannot prove they describe the current
  attempt, and the wire key's PM suffix is persisted nowhere.
- **No ledger.** The 2026-08-02 panel scored ADR-106 4–0 below the shipped
  design; it stays parked. This ADR delivers its recoverability delta for the
  findable case as a read.
- **No new schema.** 0167's observation columns carry the search bookkeeping;
  the sentinel values `search_not_found` / `search_terminal_only` are documented
  observations, not PI statuses.

## Consequences

- The parked state's floor is unchanged (ADR-107 invariants, gauge, exit); the
  ceiling improves: any parked invoice whose PI exists and gets indexed
  resolves without a human, including cases the webhook permanently lost.
- Search failures degrade to exactly today's behavior, visibly.
- One new Stripe client method and interface member; one new list + one CAS in
  the invoice store; one sweep in the reconciler; ~20 reads/sec budget shared
  with nothing else we do.
- If Stripe search is degraded during an incident, adoption is merely late —
  absence writes nothing, so a delayed index can never cause a wrong write.
