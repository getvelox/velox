# ADR-072: Outbox transport — claim-lease with detached CAS marks, derived constants

**Date:** 2026-07-03
**Status:** Accepted (shipped in P5; designed by the P5 adversarial panel — 2 lenses, both SHIP-WITH-FIXES; consolidated protocol in the remediation plan §4.5)

## Context

Both outboxes (email, webhook events) drained batches inside ONE
transaction: claim rows FOR UPDATE, run every handler (real SMTP/HTTP
round-trips), then commit all marks together. Two consequences:

1. **Lost marks re-sent delivered messages.** The 60s batch timeout
   versus a 10×~55s worst-case batch meant a slow row cancelled the tx
   after earlier rows had already been DELIVERED — their dispatched
   marks rolled back, and the next tick re-sent real invoices.
2. **The webhook event fan-out dual-wrote.** `Dispatch` committed the
   event, then each goroutine inserted its own delivery row (a crash
   between = an event no row references, never delivered, nothing to
   retry) with `next_retry_at = NULL` (the retry worker racing the
   goroutine = double-POST); the producing `webhook_outbox` row was
   marked in yet another tx (a crash between = a duplicate event with a
   fresh id receivers cannot dedupe).

## Decisions

1. **Claim-lease batches.** One short claim tx per tick:
   `attempts = attempts + 1, next_attempt_at = now() + ClaimLease`
   over `FOR UPDATE SKIP LOCKED` rows, committed immediately. The lease
   is both the crash-recovery bound and the cross-worker exclusion; the
   claim is the **single attempts-increment site** (a claim-then-crash
   row still advances toward DLQ; marks never increment).
2. **Derived constants, never hand-tuned.** Email
   `PerRowBudget = 60s` (suppression 5s — newly bounded — + branding 5s
   + dial 10s + SMTP exchange 30s + bounce report 5s, enforced by a
   per-row ctx); `ClaimLease(n) = n×PerRowBudget + 60s`;
   dispatcher defaults `BatchSize=5`, `BatchTimeout=300s`. The invariant
   chain `BatchSize×PerRowBudget ≤ BatchTimeout < ClaimLease` is
   CI-locked (`TestP5_LeaseInvariantChain`). The webhook side SPLITS
   what was one constant: `birthLeaseWindow = 120s` leases a
   freshly-created delivery row to its in-process first attempt;
   `retryClaimLease = claimBatch×13s + 60s` (claim batch 10) covers a
   claimed retry batch. One constant provably cannot serve both — sized
   for the batch it starves first delivery for ~23 minutes; sized for
   birth it double-POSTs under a slow batch.
3. **Marks are detached and CAS.** Every per-row mark runs on
   `context.WithoutCancel + 5s` — work already performed is always
   recorded, which is what actually kills the delivered-email-re-sent
   bug (the row-start budget gate is only a duplicate-window shrink) —
   and guards `WHERE status='pending'` so a stale writer (lease expired,
   a sibling already resolved the row) is dropped and logged, never
   applied. `UpdateDelivery` returns `ErrStaleDeliveryMark`; no caller
   discards mark errors anymore.
4. **Atomic fan-out + handler-owns-mark.** `DispatchFromOutbox` creates
   the event, every matching delivery row (born leased —
   `next_retry_at` is never NULL), and the producing outbox row's
   dispatched-mark in ONE tx. The outbox drainer's own success mark is a
   CAS no-op backstop. Replay batch-creates its born-leased rows in one
   tx against the already-committed clone (an interrupted replay is
   operator-visible and re-clickable).
5. **Redelivery semantics per row state**: `pending` = at-least-once —
   exactly one duplicate possible per crash/lease-expiry event;
   `dispatched` = terminal-sent; `failed` = terminal-unsent until an
   operator requeues:
   `UPDATE email_outbox SET status='pending', attempts=0, next_attempt_at=now() WHERE id='…' AND status='failed'`.
   Permanent errors (payload decode, suppressed recipient, permanent
   SMTP 5xx) go to `failed` immediately instead of riding 15 backoff
   slots over ~72h.
6. **One SMTP path, strict by default.** All three modes flow through a
   single `deliver()`: `DialContext(10s)`, exchange deadline
   `min(30s, ctx remaining)`, **strict STARTTLS** (the old
   `smtp.SendMail` path upgraded opportunistically — a MITM stripping
   the advertisement silently downgraded invoice traffic to plaintext),
   AUTH only when configured AND advertised, `SMTP_TLS=none` forbidden
   when `APP_ENV=production` (fails loudly at send). **Breaking**: a
   config pointing `starttls` at a no-TLS sandbox now errors — set
   `SMTP_TLS=none` explicitly (local compose/docs already do); the boot
   log prints the active mode for self-diagnosis.
7. **Retry worker leader-gated** (`LockKeyWebhookRetry`) — the claim
   lease alone is a correct multi-replica guard, but gating makes its
   sizing non-critical, matching both outbox dispatchers.

## Accepted residuals

- **Deferred from P5's Closes list (recorded per the amend-decisions
  rule): `outbox_sender.go` raw tokens at rest.** Password-reset and
  member-invite emails store their single-use token URLs in
  `email_outbox.payload` jsonb until dispatch (and after: see the
  2026-08-30 amendment — no retention window exists). The
  fix is payload-field encryption (the customer-PII encryptor is the
  natural tool) — deferred as a named backlog item rather than rushed
  into this transport rewrite; mitigations today: tokens are single-use,
  reset tokens expire in 1h, and DB access is already the crown jewels.
- Pre-P5 pending webhook deliveries may carry NULL `next_retry_at`; the
  claim SQL's NULL branch is retained (with a legacy-rollout comment +
  regression test) until no such rows can exist.
- The retry path re-signs with a fresh timestamp per attempt (unchanged
  behavior); receivers must tolerate at-least-once delivery, as
  documented since ADR-040.

## Test locks (real Postgres, mutation-verified 2026-07-03)

Email: lease-invariant chain; mark survives batch-ctx death (mutation:
mark on batch ctx → fails); permanent-error immediate DLQ; panic row
doesn't strand the batch; two concurrent claimers disjoint over 10 rows;
strict-STARTTLS refusal against a live no-TLS fake + prod-none
rejection. Webhook: event+deliveries one tx born-leased (retry worker
can't steal inside the birth lease); UpdateDelivery CAS drops a stale
failure-mark after a sibling's success (mutation: guard removed →
fails); handler-owns-mark (outbox row dispatched atomically, replayed
tick re-claims nothing, exactly one event). Pre-existing claim-lease +
NULL-branch regression suites stay green.

**Amendment 2026-08-30 — the webhook-outbox drainer's claim is now pinned
too.** The 2026-07-03 locks above cover the email outbox and the webhook
retry worker; `webhook.OutboxStore.ProcessBatch` (the `webhook_outbox`
drainer) had no concurrent-claimer test — its `FOR UPDATE SKIP LOCKED` +
lease claim was held by convention. Two real-Postgres tests in
`internal/webhook/outbox_integration_test.go` close that:
`TestOutbox_ProcessBatch_SkipsRowsLockedBySibling` (a sibling transaction
holds row locks on half the due set; the claim must neither block nor
take those rows, and must take exactly them after release) and
`TestOutbox_ProcessBatch_ConcurrentClaimersDisjoint` (two dispatchers race
the same 10 rows; every row attempted exactly once). Mutation matrix
against the real store: deleting `FOR UPDATE SKIP LOCKED`, or keeping
`FOR UPDATE` without `SKIP LOCKED`, turns the sibling test red (claim blocks
until the 5 s ctx expires) while the race test stays green; dropping the
lease stamp from the claim UPDATE turns the race test red (rows attempted
twice) while the sibling test stays green — each mechanism has exactly one
test that fails for it.

## Amendment 2026-08-30 — no 30-day retention exists

The residual above said dispatched `email_outbox` rows carry a "+30-day
retention". Nothing implements one: the only 30-day figure in the code is
`ListByCustomer`'s read window (`internal/email/outbox.go`), not a
deletion. Dispatched rows persist indefinitely — and must, because they
are the invoice-email delivery record (the invoice timeline's
`ListByInvoice` has no age bound; the Postmark bounce/delivery webhook
correlates on the row id). So the token-URL exposure is unbounded in time
and the fix is a post-dispatch scrub of secret-bearing payload fields, not
row deletion; the mitigations listed (single-use tokens, 1 h reset expiry)
stand. `webhook_outbox` dispatched rows are pure transport intent —
`webhook_events`/`webhook_deliveries` are the replayable record — and are
safe to prune at any age; Velox does not do so automatically (register:
outbox-pruner, trigger recorded in velox-ops).

