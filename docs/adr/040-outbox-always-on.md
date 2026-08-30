# ADR-040: Outbox is the only path (webhook + email)

**Status:** Accepted
**Date:** 2026-05-30
**Supersedes:** the boot-time fallback contract introduced when
the webhook/email outboxes shipped (the `VELOX_WEBHOOK_OUTBOX_ENABLED`
and `VELOX_EMAIL_OUTBOX_ENABLED` env flags).

## Context

Velox shipped transactional outboxes for outbound webhooks
(`webhook_outbox`) and customer-facing email (`email_outbox`) to make
delivery durable across crashes and transient downstream failures.
Producers persist an event/email intent inside the business-op
transaction; a background dispatcher drains the queue with retry +
DLQ.

> **Amendment (2026-08-30): email is post-commit; webhooks are in-tx.**
> The paragraph above is true of `webhook_outbox` — every producer
> enqueues inside the business transaction (`invoice/postgres.go`,
> `subscription/postgres.go`, `credit/postgres.go`). It has never been
> true of `email_outbox`: all ten `OutboxSender` sites enqueue via
> `OutboxStore.EnqueueStandalone`, which opens its own transaction
> AFTER the business transaction commits; the in-tx primitive
> `Enqueue(ctx, tx, …)` has no production caller besides
> `EnqueueStandalone` itself. The email outbox is therefore a durable
> at-least-once delivery queue *from the moment its row commits*, and
> the commit→enqueue gap (a process death or database failover between
> the two commits) loses the email with the state standing — the
> exactly-once gates on the money path then suppress every later
> attempt. Under the N ≥ 2 production posture that gap is hit by routine
> failovers, so the money emails move into the state-transition
> transaction. **Shipped 2026-08-31 for the dunning pair** (warning +
> escalation): `UpdateRunIfActive` takes a `then` hook that runs the
> outbox enqueue inside the state transaction, under a SAVEPOINT — an
> email-side failure logs loud and skips the email, never the money
> write. Payment receipt + payment failed ride the settle path and move
> in their own PR; account-plane and operator-initiated sends stay
> post-commit (the operator receives the enqueue error synchronously).

At migration time, both outboxes shipped behind boot env flags
(`VELOX_WEBHOOK_OUTBOX_ENABLED=true` and `VELOX_EMAIL_OUTBOX_ENABLED
=true`) so operators could roll back to the legacy direct-dispatch
path on regression. The router used the flag to switch BOTH the
producer wiring (`OutboxSender` ↔ `*email.Sender`, `OutboxDispatcher`
↔ direct `Service.Dispatch`) AND the dispatcher worker startup
(skipped when the flag was off, since nothing would be enqueued).

Three observations on 2026-05-30 forced a re-evaluation:

1. **The flag couples producer and consumer.** There's no way to
   independently disable the dispatcher (e.g. for the FLOW E7 test
   that wants to observe a pending row sit in `email_outbox` while
   the dispatcher is paused). The same env var moves both ends
   together.
2. **The "off" path silently weakens correctness.** With the flag
   off, the same Go call site (`SendPaymentReceipt`, `Dispatch`,
   etc.) becomes a one-shot synchronous call. Transient SMTP / HTTP
   5xx failures bubble out as errors that the caller logs and
   discards — the email or webhook is lost forever. The flag
   trades durability for nothing in return, and the trade is
   invisible at the call site.
3. **Boot-time flags are the wrong tool for the operational concerns
   they're meant to address.** Multi-replica safety is already
   handled by the dispatcher's advisory-lock gating
   (`LockKeyWebhookDispatcher` / `LockKeyEmailDispatcher` —
   `internal/platform/postgres/advisory_lock.go`). Runtime pause
   ("stop sending email RIGHT NOW") wants a runtime API or external
   advisory-lock hold, not a restart-required env flag. Worker /
   API binary split is a deployment topology decision (`cmd/velox-worker`
   if it ever ships), not a feature flag.

The flag also violates two project principles:

- **`feedback_no_silent_fallbacks`** — engine paths that can't
  produce a correct answer must fail loudly, not substitute a
  default. The direct-dispatch / direct-SMTP path on a transient
  failure silently drops the side-effect.
- **`feedback_no_belt_and_suspenders`** — one path unless
  independent failure modes warrant two. Outbox failure modes (DB
  unreachable, dispatcher crashed) are NOT independent of direct-
  send failure modes (SMTP unreachable, sender crashed). One can't
  rescue the other.

## Decision

**Cut both flags. The outbox is the only path.**

- `VELOX_WEBHOOK_OUTBOX_ENABLED` removed. Producers always enqueue
  to `webhook_outbox`; the dispatcher worker always starts.
- `VELOX_EMAIL_OUTBOX_ENABLED` removed. Producers always enqueue
  to `email_outbox` via `OutboxSender`; the dispatcher worker
  always starts.
- `Server.OutboxEnabled` and `Server.EmailOutboxEnabled` fields
  removed from `internal/api/router.go`.
- Boot-time WARN logs ("outbox DISABLED — using legacy direct-
  dispatch path") removed — there's no longer a DISABLED state.
  The boot-time INFO logs ("outbox enabled — producers will
  enqueue…") also removed; the outbox being on is now an
  invariant, not a state worth logging.
- `.env.example` flag entries removed.
- The direct-SMTP `*email.Sender` stays — but only the dispatcher
  worker calls it now (its real purpose: convert an outbox row
  into an actual SMTP send). Same for `webhook.Service.Dispatch`:
  only the dispatcher calls it now.

## Runtime concerns the flag was meant to address

| Concern | What the flag tried | New answer |
|---|---|---|
| Multi-replica: only one drains | Disable on N−1 replicas | Advisory lock already handles this; first claimant wins, all others sleep |
| Emergency pause | Set flag false + restart | Hold the advisory lock from an external psql session (no restart) |
| Separate worker process | Skip the goroutine on API replicas | When/if this is needed: split into `cmd/velox` (API) + `cmd/velox-worker` (dispatcher-only); topology in deploy, not code |
| Local dev quiet | Set flag false | No traffic = no rows = quiet dispatcher (it polls every 5s, claims nothing) |

All four are dynamic OR deployment-shape concerns; none of them
need a boot env flag.

## Consequences

- **Operators lose no real capability.** The flag was never set to
  `false` in any deployment; `.env.example` shipped with both at
  `true`. There's no DP and no production user to break.
- **Test surface area halves.** Every email / webhook producer
  change had to be considered against both paths; now there's one.
- **Boot complexity drops.** Router wiring loses two env-parse
  blocks + their dual-branch if/else; the seven typed interface
  variables collapse from `var X type; if … { X = a } else { X = b }`
  shape to one-line declarations.
- **FLOW E7 becomes runnable cleanly.** The test was effectively
  blocked on the absence of a separate dispatcher-disable flag.
  The advisory-lock pause technique works today and is documented
  in the updated FLOW.
- **Future operator-pause API.** When a DP names "I want to pause
  email delivery for tenant X mid-flight," the right place to add
  that is a runtime API (per-tenant flag in `tenant_settings`,
  honored by the dispatcher when claiming rows) — not a return
  of the boot flag.
- **Migrations / data unchanged.** `webhook_outbox` and
  `email_outbox` schemas, their dispatchers, DLQ tables, advisory-
  lock keys all stay as-is.

## What stays available for legitimate use

- `pg_advisory_lock(76540004)` from any psql session: pauses the
  email dispatcher cluster-wide until released.
- `pg_advisory_lock(76540003)` (or whatever the webhook lock key is):
  same for webhook dispatcher.
- These ARE the runtime pause primitives. They were already there;
  the flag was never the right interface.

## Revisit trigger

Resurrect the flag (or a runtime equivalent) when:
- A DP names a use case where producer enqueue must be skipped
  cluster-wide (e.g. compliance regime requires "no email storage
  even transient"), OR
- A binary split (`cmd/velox-worker`) is shipped and one side
  needs to know which role it's running.

Until then, "outbox on" is an invariant.

## Industry references

- Stripe, Lago, Chargebee, Recurly: none expose a boot-time flag
  that switches email/webhook delivery from outbox-backed to
  direct. Pause is a runtime per-endpoint API.
- Sidekiq, Hangfire, Resque (background-job ecosystems): split
  binary (worker mode vs web mode), not env-flag. Always-on within
  the role.
- PostgreSQL `pg_advisory_lock` is the canonical primitive for
  external runtime pause of a database-backed worker; it's what
  the dispatcher already uses for multi-replica gating.
