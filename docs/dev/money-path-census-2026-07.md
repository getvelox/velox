# Money-path census (2026-07)

A dated census, moved verbatim from the [Money-Path Robustness Playbook](money-path-robustness-playbook.md); its § numbers refer to that playbook.

<!-- vale off -->
<!-- Recorded text, kept as written (docs/dev/writing-style.md: do not rewrite records). -->

## 6. Current posture (as of 2026-07-11)

**Update (2026-07-11).** Since the 2026-07-02 census below: the simulated-data
lifecycle became its own design-of-record (**ADR-086** — is_simulated gates on
all five wall-clock money sweeps, clock teardown, branded `EffectiveNow` on the
FE; supersedes ADR-016); the HA/N=2 posture is documented in
`ha-readiness-2026-07-06.md` (10 hazards, 17 verified-safe; build trigger =
production cutover); and the 2026-07-10 design-quality review
(`design-review-2026-07-10.md`) shipped a fix-now batch (#427–#440) including
the `domain.TaxFacts` embed (class-A/B adjacent: the tax-fact field-drop class
is now a compile error), typed effect outcomes (`domain.NotifyOutcome` — a
skipped effect is never a silent nil), and fail-loud parity on the cancel
builder. A proposed **rendered/asserted-truth class** and the
containment trip-wire live in that review, still pending adoption here (the
review called it "class J", but that letter has since been taken by the adopted
class J, contracted-instant stamping — see §1).

## 6a. Census posture (as of 2026-07-02)

Classes **A** (exactly-once), **B** (money-event dual-writes), **D** (concurrency
C1–C4), and **H** (tenant isolation) are locked and test-covered — don't
re-litigate them. Every MEDIUM finding from the original census (the full sweep
of the codebase against the failure-class map above) has shipped:

- ~~`subscription.Activate` lost-update~~ — **fixed #327**: `Update` now carries
  `AND status='draft'` + an `ErrNoRows` re-query → `InvalidState` conflict,
  matching the `transitionInTx` chokepoint its siblings use. Locked by the
  real-Postgres `TestActivate_StoreUpdate_GuardsAgainstConcurrentCancel`
  (mutation-verified).
- ~~`SettleFailed` dunning crash-window~~ — **fixed #328**: the `dunning_backfill`
  reconciler (`Engine.EnrollFailedWithoutDunning`) re-drives the idempotent
  `StartDunning` for failed invoices with no run (state-agnostic `NOT EXISTS`,
  0085-exactly-once). **ADR-064** ratifies the triggered-primary +
  derived-backstop architecture; **#330** additionally moved the `payment.failed`
  event into the fail-tx (outbox, gated on `firstForThisPI`).
- ~~`dashboard_sessions` no RLS~~ — **fixed #331 (m0124)**, which also fenced
  `user_tenants` — a second unfenced table **found by the new
  `TestRLSIsolation_EveryTenantTableIsFenced`** (the §5.2 enumeration test, now
  built: discovers every `tenant_id` table from `information_schema`, asserts
  `ENABLE`+`FORCE`+policy, empty reason-required allowlist). The manual audit
  sweep had missed `user_tenants` — enumeration beats lists; the whole
  missing-RLS class is now CI-caught.

The LOW residue is closed too — **the census is fully discharged**:

- ~~`UpsertPolicyTx` validation parity~~ — **fixed #333**: both policy writers
  route through one shared `normalizeAndValidatePolicy` chokepoint, so a
  mismatched recipe fails at instantiate-time, not mid-campaign. Locked by
  `TestUpsertPolicyTx_ValidationParity` (mutation-verified).
- ~~64KB webhook-body truncation~~ — **fixed #334**: an over-cap body is
  detected (read cap+1) and rejected as 413 `payload_too_large` with a size
  diagnostic, instead of truncating → HMAC-failing → a misleading
  "invalid signature" 400. Locked by
  `TestWebhookHandler_OversizedBodyIs413NotSignatureFailure` (mutation-verified).

Deferred-with-trigger and honestly documented (do not re-flag): the failed
customer **email** post-commit best-effort (by design — symmetric to the receipt
email; the *event* is in-tx since #330); `EventDispatcher.Dispatch` no-`*sql.Tx`
for ~16 *notification* webhooks (zero consumers, no money event affected);
`relieveUnpaidPrebill` unpaid-branch post-commit; exhaustRun's 24h self-heal
re-attempting a permanently-failing mover unbounded (the deliberate
"keep requeryable" tradeoff); `RetryPendingTaxCommitForClock` absent (test-mode
only — clock-pinned ⇒ `livemode=false` by CHECK constraint, no real-VAT exposure);
a durable `collection_failed_at` anchor + the ADR-062 queue (see ADR-064's
cheap-strengthening triggers).
