package billing

import (
	"context"
	"log/slog"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
)

// Reconciler is one recovery sweep: it scans for stuck/failed post-commit
// effects and re-drives each, idempotently, every tick. Reconcile returns
// (advanced, errs): advanced counts real work done; per-row errs are collected,
// never abort the batch, and the same item may be handed back next tick.
//
// ELIGIBILITY RULE — audited across all seven sweeps 2026-07-30. The predicate
// must survive the very crash it recovers from, which permits two shapes and
// forbids a third:
//
//  1. STRUCTURAL — the missing effect IS the predicate; no marker exists.
//     tax_commit (has tax_calculation_id, no tax_transaction_id),
//     tax_reversal (voided/uncollectible, tax_transaction_id set,
//     tax_reversed_at IS NULL), dunning_backfill (finalized+failed with
//     NOT EXISTS a dunning run). Strongest: nothing had to be written for
//     the row to become visible.
//
//  2. CO-TRANSACTIONAL MARKER — a flag written in the SAME tx as the
//     obligation it describes, so it cannot be absent while the obligation
//     exists. clawback_issue's issue_pending is created in-tx with the item
//     change; tax_retry's tax_status/tax_error_code are set on the invoice
//     struct before its INSERT. Equivalent safety: partial state is
//     unreachable, so there is nothing for a backstop to catch.
//
//  3. POST-HOC MARKER — written in a separate write AFTER the obligation
//     committed, so it can fail on its own and strand the row. NOT sufficient
//     alone; always pair it with a structural branch. There is exactly one
//     post-hoc case, and it is the exemplar:
//     ListPendingCreditNoteTaxReversal = tax_reversal_pending OR the
//     structural over-remit state.
//
// Adding a sweep? Say which shape it is. If the honest answer is (3), the
// structural branch is not optional.
//
// Two known bounds, deliberate rather than accidental: the structural branches
// carry a 24h freshness window (anti-churn, #310) that bounds DETECTION only —
// cn_tax_reversal promotes a marker-less orphan onto its unbounded marker
// branch on the first failed re-drive (2026-08-30), so an orphan is never aged
// out of recovery, only out of the cheap scan; and payment_unknown
// keys on a payment_status the CHARGE path must have stamped — a crash before
// that stamp is recovered by the auto-charge retry's stable Stripe idempotency
// key instead, not by this sweep.
//
// This is the seam the future obligation-queue drainer (ADR-062) plugs into:
// when the four re-drive sweeps migrate onto the generalised webhook_outbox, the
// drainer is just one more Reconciler in s.reconcilers() — the driver below,
// the leader gate, the mode fan-out, the logging, and the metric are reused
// verbatim. (That is why this is a single-method interface, not a Source/Drive
// split: the queue collapses the per-effect scans into one drainer, so a
// per-effect Source seam would be discarded when (c) lands.)
type Reconciler interface {
	Name() string
	Reconcile(ctx context.Context, batch int) (int, []error)
}

// reconcilerFunc adapts an existing RetryPendingX/Run method into a named
// Reconciler. The re-drive body and its eligibility SQL stay UNCHANGED in their
// own service/store (with their load-bearing comments); this is a presentational
// shim, no behaviour.
type reconcilerFunc struct {
	name string
	fn   func(ctx context.Context, batch int) (int, []error)
}

func (r reconcilerFunc) Name() string { return r.name }
func (r reconcilerFunc) Reconcile(ctx context.Context, batch int) (int, []error) {
	return r.fn(ctx, batch)
}

// recordReconcilerSweep is the metric sink, a var so tests can observe it.
var recordReconcilerSweep = mw.RecordReconcilerSweep

// reconcilers returns the recovery sweeps in their deliberate tick order. THE
// ORDER IS LOAD-BEARING: the tax reconcilers must run before auto-charge so a
// freshly-finalised invoice's finalize-time auto-charge doesn't slip a tick, and
// payment_unknown runs first so a stuck-unknown charge that actually succeeded is
// marked paid before the retry path considers re-charging. An order-asserting
// test guards this. Only wired (non-nil) reconcilers are included.
func (s *Scheduler) reconcilers() []Reconciler {
	var rs []Reconciler
	if s.paymentReconciler != nil {
		// payment_unknown is a STATE-SYNC against Stripe (3-valued:
		// succeeded/failed/still-in-flight), NOT a re-drive of a local failed
		// effect. It rides this driver only for uniform log+metric and will
		// NEVER migrate to the obligation queue (ADR-062) — its truth lives at
		// Stripe, there is no local obligation to enqueue.
		rs = append(rs, reconcilerFunc{"payment_unknown", s.paymentReconciler.Run})
	}
	if s.taxRetrier != nil {
		rs = append(rs,
			reconcilerFunc{"tax_retry", s.taxRetrier.RetryPendingTax},
			reconcilerFunc{"tax_commit", s.taxRetrier.RetryPendingTaxCommit},
			reconcilerFunc{"tax_reversal", s.taxRetrier.RetryPendingTaxReversal},
		)
	}
	if s.clawbackRetrier != nil {
		rs = append(rs,
			reconcilerFunc{"clawback_issue", s.clawbackRetrier.RetryPendingClawbackIssue},
			reconcilerFunc{"cn_tax_reversal", s.clawbackRetrier.RetryPendingCreditNoteTaxReversal},
		)
	}
	if s.engine != nil {
		// dunning_backfill runs LAST — an order-independent backstop. The invoice
		// is already terminally failed, so no earlier sweep touches it; it re-drives
		// the idempotent StartDunning for invoices SettleFailed left failed-but-
		// undunned (post-commit crash / exhausted retry). Kept out of the hot fail
		// path deliberately: folding StartDunning into the fail-tx would hold the
		// invoice FOR UPDATE across StartDunning's ~600ms retry sleep + a cross-
		// domain policy read (see the design panel for SettleFailed).
		rs = append(rs, reconcilerFunc{"dunning_backfill", s.engine.EnrollFailedWithoutDunning})
	}
	return rs
}

// runReconcilers runs each sweep in order under the caller's already-leader-gated,
// mode-scoped ctx, with one uniform log + the per-reconciler sweep metric. A
// per-row error is logged and metered but never aborts the batch or the next
// reconciler. This replaces six near-identical copy-pasted log blocks.
func runReconcilers(ctx context.Context, mode string, rs []Reconciler, batch int) {
	for _, r := range rs {
		if r == nil {
			continue
		}
		n, errs := r.Reconcile(ctx, batch)
		if n > 0 || len(errs) > 0 {
			slog.Info("reconciler swept", "reconciler", r.Name(), "mode", mode, "advanced", n, "errors", len(errs))
		}
		for _, e := range errs {
			slog.Error("reconciler error", "reconciler", r.Name(), "mode", mode, "error", e)
		}
		recordReconcilerSweep(r.Name(), mode, n, len(errs))
	}
}
