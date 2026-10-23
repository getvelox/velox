package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// thresholdEval is the decision the scan reaches for one subscription. When
// CrossedAny is true the engine fires an early finalize via fireThreshold;
// otherwise the row is left alone until the next tick. RunningSubtotal and
// PerItemRunning are kept in the struct so the firing path can reuse the
// already-computed lines instead of re-aggregating after the decision.
type thresholdEval struct {
	CrossedAny    bool
	CrossedAmount bool
	CrossedItem   bool
	CrossedItemID string
	// RunningSubtotal is the amount_gte comparison figure — the customer's
	// committed spend, including non-additive buckets that bill at cycle
	// close instead of riding this invoice. Feeds the cap check
	// and the threshold_crossed event payload ONLY.
	RunningSubtotal int64
	// BilledSubtotal is the sum of LineItems' amounts — what the invoice
	// actually charges. Feeds tax + the invoice header. Diverges from
	// RunningSubtotal exactly when a non-additive bucket is dropped
	// (ADR-066 §4).
	BilledSubtotal  int64
	LineItems       []domain.InvoiceLineItem
	InvoiceCurrency string
	// Unpriced is the usage this threshold invoice is built without because
	// no price matched it, and that no later invoice reports. It follows
	// the line rules below: a non-additive bucket belongs to the period
	// close (its full-window pass reports it), so it is left out here;
	// everything else is this invoice's to report (#981).
	Unpriced []leftOutBucket
	// BelowZero is the usage this threshold invoice counts as 0 because it
	// was below zero after corrections (#1193), under the same rules as
	// Unpriced. The fire writes it on its transaction (net_negative.go).
	BelowZero []leftOutBucket
}

// nonAdditive reports whether usage under a meter's aggregation cannot be
// split across a threshold fire and a cycle close (max[0,t1) + max[t1,end) ≥
// max[0,end); last is a point-in-time read). ADR-066 §4. A meter has one
// aggregation (ADR-130 §2), so this is a property of the meter. An empty
// aggregation reads as sum, as billing reads it.
func nonAdditive(aggregation string) bool {
	return aggregation != "" && !domain.AdditiveAggregation(aggregation)
}

// ScanThresholds finds every subscription with a billing threshold configured
// and fires an early-finalize invoice for each one whose in-cycle running
// total has crossed the cap. Called by the scheduler between the auto-charge
// retry sweep and the natural cycle scan so a threshold-fired invoice is
// resolved (charge + dunning entry) on the same tick it's emitted.
//
// Invariants:
//
//   - Idempotent under retry. The partial unique index on
//     invoices(tenant_id, subscription_id, billing_period_start) WHERE
//     billing_reason='threshold' guarantees at most one threshold invoice
//     per cycle. A second tick that observes the same crossed state lands
//     on errs.ErrAlreadyExists and short-circuits.
//
//   - Mode-scoped via ctx livemode. The scheduler fans out per-mode before
//     calling, so the candidate fetch (ListWithThresholds) honours the same
//     RLS partition as the natural cycle scan.
//
// Returns the count of fired invoices and any errors encountered. Per-sub
// errors do not abort the whole scan — they're collected and returned so
// the operator dashboard can flag stuck rows without losing throughput.
func (e *Engine) ScanThresholds(ctx context.Context, batchSize int) (int, []error) {
	if batchSize <= 0 {
		batchSize = 50
	}

	// Cursor-drain the WHOLE candidate set. Pre-fix this fetched a single
	// `ORDER BY s.id LIMIT batch` page per tick with no cursor: the candidate
	// set never drains (a fired sub still has thresholds configured), so subs
	// beyond the first batch were NEVER scanned — spend caps silently disabled
	// past batchSize subscriptions. The strictly-increasing id cursor makes the
	// loop terminate by construction (no max-pages belt-and-suspenders), and it
	// advances past failing subs so one bad row can't wedge the scan or cause a
	// re-evaluation within the same tick. The fire-once probe in
	// scanOneThreshold is what keeps a full drain affordable — already-fired
	// subs cost one indexed lookup, not a previewWithWindow aggregation.
	fired := 0
	var errsOut []error
	afterID := ""
	for {
		if err := ctx.Err(); err != nil {
			errsOut = append(errsOut, fmt.Errorf("threshold scan ctx done: %w", err))
			break
		}
		candidates, err := e.subs.ListWithThresholds(ctx, postgres.Livemode(ctx), afterID, batchSize)
		if err != nil {
			errsOut = append(errsOut, fmt.Errorf("list candidates: %w", err))
			break
		}
		if len(candidates) == 0 {
			break
		}

		for _, sub := range candidates {
			didFire, err := e.scanOneThreshold(ctx, sub)
			if err != nil {
				slog.Error("threshold scan failed for subscription",
					"subscription_id", sub.ID,
					"tenant_id", sub.TenantID,
					"error", err,
				)
				errsOut = append(errsOut, fmt.Errorf("subscription %s: %w", sub.ID, err))
				continue
			}
			if didFire {
				fired++
			}
		}

		afterID = candidates[len(candidates)-1].ID
		if len(candidates) < batchSize {
			break // short page = set drained
		}
	}

	if fired > 0 {
		slog.Info("threshold scan fired invoices", "count", fired)
	}
	return fired, errsOut
}

// ScanThresholdsForClock is the catchup-path counterpart to
// ScanThresholds. ADR-029 Phase 3: the wall-clock cron's
// ListWithThresholds excludes clock-pinned subs; this method is the
// disjoint per-clock entry point called by the catchup orchestrator
// after period generation but before charge retry.
//
// Per-sub scan logic is identical to the cron path — the only
// difference is the candidate-fetch scope. Errors collected per-sub,
// not aborted-on.
func (e *Engine) ScanThresholdsForClock(ctx context.Context, tenantID, clockID string, batchSize int) (int, []error) {
	if batchSize <= 0 {
		batchSize = 50
	}
	// Same cursor-drain as ScanThresholds — a clock with more than batchSize
	// threshold subs must scan all of them in one Advance.
	fired := 0
	var errsOut []error
	afterID := ""
	for {
		if err := ctx.Err(); err != nil {
			errsOut = append(errsOut, fmt.Errorf("threshold scan ctx done: %w", err))
			break
		}
		candidates, err := e.subs.ListWithThresholdsForClock(ctx, tenantID, clockID, afterID, batchSize)
		if err != nil {
			errsOut = append(errsOut, fmt.Errorf("list threshold candidates for clock %s: %w", clockID, err))
			break
		}
		if len(candidates) == 0 {
			break
		}

		for _, sub := range candidates {
			didFire, err := e.scanOneThreshold(ctx, sub)
			if err != nil {
				slog.Error("threshold scan failed for clock-pinned subscription",
					"subscription_id", sub.ID,
					"tenant_id", sub.TenantID,
					"clock_id", clockID,
					"error", err,
				)
				errsOut = append(errsOut, fmt.Errorf("subscription %s: %w", sub.ID, err))
				continue
			}
			if didFire {
				fired++
			}
		}

		afterID = candidates[len(candidates)-1].ID
		if len(candidates) < batchSize {
			break
		}
	}
	if fired > 0 {
		slog.Info("threshold scan fired invoices on advance", "clock_id", clockID, "count", fired)
	}
	return fired, errsOut
}

// scanOneThreshold evaluates a single subscription. Builds running line items
// over the partial cycle [period_start, now), checks each configured cap,
// and — when crossed — fires the early finalize via fireThreshold.
func (e *Engine) scanOneThreshold(ctx context.Context, sub domain.Subscription) (bool, error) {
	if sub.BillingThresholds == nil {
		return false, nil
	}
	// Only ACTIVE subs fire thresholds. Trialing is skipped for the same reason
	// the natural cycle skips it — there's nothing billable, so a threshold
	// "cross" during trial would just emit a zero-amount draft; defer until the
	// status flips. (ListWithThresholds returns active+trialing, hence the
	// explicit gate here.)
	if sub.Status != domain.SubscriptionActive {
		return false, nil
	}
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return false, nil
	}
	now := e.effectiveNow(ctx, sub)
	// pause_collection neuters the financial side; firing a threshold under
	// pause would emit a draft that can't be charged or dunned. Skip — the
	// scan resumes when collection does: at resumes_at, even while the row
	// still carries a timed pause waiting on a paused period's close.
	if sub.PauseCollection.InForceAt(now) {
		return false, nil
	}
	periodStart := *sub.CurrentBillingPeriodStart
	if !now.After(periodStart) {
		return false, nil
	}

	// Boundary SKIP: once `now` reaches the period end, the threshold window is
	// closed — do not fire. Pre-fix, a crossing first observed on/after the
	// boundary tick (scheduler downtime, or the crossing landing in the last
	// inter-tick window) fired with a [periodStart, now) window that spilled
	// into the NEXT period; the cycle close's watermark is scoped to THIS
	// period's threshold invoices, so the spilled usage was billed again —
	// double-billing across the boundary. The same-tick natural cycle scan
	// (scheduler step order: thresholds before cycle close, one goroutine)
	// bills the whole elapsed period through the full-fidelity path instead
	// (prorated segments, item changes, terminal-cancel handling — fireThreshold
	// has none of those). Correctness no longer rests on that ordering: every
	// fire and every close proves its period snapshot under the row lock
	// (ADR-115); the skip is an optimization that keeps boundary crossings on
	// the full-fidelity path. We deliberately do NOT clamp-and-fire: money-correct,
	// but it would bill exactly the mid-period-swap/scheduled-cancel subs
	// through the crude path. Accepted semantic: no subscription.threshold_
	// crossed event for a boundary-observed crossing — the cycle invoice is the
	// operator-visible artifact (see ADR-065).
	if !now.Before(*sub.CurrentBillingPeriodEnd) {
		return false, nil
	}
	// A due cancel_at ends the period early, so it gets the same skip. The
	// billing run's cancel arm bills [period_start, cancel_at) at the
	// contracted instant (ADR-097), but the scheduler runs this scan first in
	// its tick: a fire here would bill [period_start, now), usage stamped after
	// the subscription ended included, and the
	// cancel's final invoice would then find the window already billed.
	if sub.CancelAt != nil && !now.Before(*sub.CancelAt) {
		return false, nil
	}

	// Fire-once probe: if a threshold invoice already exists for this cycle,
	// this sub is done until the period rolls. Pre-fix the scan re-evaluated
	// and re-fired every tick after a crossing: each re-fire burned an invoice number + a paid Stripe Tax
	// calculation before the dedup index rejected it (~600/cycle). The probe
	// also skips the expensive previewWithWindow aggregation for already-fired
	// subs — what makes the drained scan affordable.
	//
	// This probe is a check-then-act OPTIMIZATION, not the exactly-once
	// mechanism: two concurrent scans can both pass it, and the partial unique
	// index (idx_invoices_threshold_unique_per_cycle, 0056: one threshold
	// invoice per sub+period_start) remains the correctness seam — the loser
	// lands on errs.ErrAlreadyExists and short-circuits, exactly as before.
	//
	// The probe sees the cycle's threshold invoice in ANY status, not the
	// watermark: the index holds the cycle's one slot whatever the status,
	// so after a void (or a write-off) no later fire this cycle can insert.
	// Probing with the watermark re-ran the evaluation and a paid tax
	// calculation every tick after a void, only for the index to refuse the
	// insert. A voided fire's window is billed by the cycle close instead.
	//
	// The probe fetches the full row (not just the watermark instant) so a
	// hit can heal the $0/credited MarkPaid crash window: a crash between
	// invoice-create and MarkPaid re-enters HERE on every later tick — the
	// only re-entry this sub gets — so this is where the stranded
	// payment_pending row is repaired (ADR-066).
	if existing, err := e.invoices.GetThresholdInvoiceForCycleAnyStatus(ctx, sub.TenantID, sub.ID, periodStart, *sub.CurrentBillingPeriodEnd); err == nil {
		e.healStrandedZeroDue(ctx, sub.TenantID, existing, now)
		return false, nil // already fired this cycle
	} else if !errors.Is(err, errs.ErrNotFound) {
		// Scanning blind risks the exact re-fire burn the probe exists to stop —
		// fail loud, retry next tick (feedback_no_silent_fallbacks).
		return false, fmt.Errorf("probe threshold invoice for cycle: %w", err)
	}

	eval, err := e.evaluateThresholds(ctx, sub, periodStart, now)
	if err != nil {
		return false, fmt.Errorf("evaluate: %w", err)
	}
	if !eval.CrossedAny {
		return false, nil
	}

	// Empty billable set (ADR-066 §4b): the cap crossed but every billable
	// line was a dropped non-additive bucket (a pure-max/last sub). There is
	// nothing to invoice — the cycle close bills those
	// buckets full-period. Skip BEFORE fireThreshold so no invoice number is
	// minted and no paid tax calculation runs (the pre-fix currency bail sat
	// after both, erroring every tick), with a once-per-(sub, period)
	// operator artifact so the crossing isn't silent: the Spend-thresholds
	// card says a cap exists; the timeline must say why no invoice appeared.
	if len(eval.LineItems) == 0 {
		e.noteThresholdDeferred(ctx, sub, eval, periodStart)
		return false, nil
	}

	fired, err := e.fireThreshold(ctx, sub, eval, periodStart, now)
	if err != nil || !fired {
		return fired, err
	}
	// The threshold invoice committed without this usage. Before #981 the
	// preview's warnings were dropped here, so nothing said so; the period
	// close then bills only from this invoice's end, so this usage is billed
	// nowhere. Same WARN as the cycle path, and counted once, after the
	// commit (a losing concurrent fire returns fired=false and counts
	// nothing).
	unpriced := unpricedUsage{}
	for _, u := range eval.Unpriced {
		slog.Warn("threshold: usage matched no rating rule and meter has no default binding — not billed",
			"meter", u.EventName, "quantity", u.Quantity.String(),
			"subscription_id", sub.ID)
		unpriced.add(u.MeterID, u.Quantity)
	}
	unpriced.record(ctx, sub)
	// Usage below zero: its audit row committed with the fire (#1193).
	for _, b := range eval.BelowZero {
		slog.Warn("threshold: usage below zero after corrections — billed as 0",
			"meter", b.EventName, "quantity", b.Quantity.String(),
			"subscription_id", sub.ID)
	}
	return true, nil
}

// thresholdWatermark is one close window's threshold-fire ground truth
// (ADR-066 §4) — shared by BOTH period-closers (billOnePeriod and
// billFinalOnImmediateCancelImpl) so the protocol cannot drift between them:
//
//   - billedThrough: usage before this instant already billed on the
//     mid-cycle threshold invoice; additive buckets bill only the residual
//     window.
//   - lines: the fire invoice's persisted line items — the per-bucket
//     ground-truth for the non-additive clamp exemption. A max/last bucket
//     with NO line was deliberately deferred by the fire and must bill
//     full-window at close; one WITH a line already billed its window.
//     Keyed on the invoice, never the mutable threshold config — an
//     operator removing thresholds between fire and close would otherwise
//     resurrect the double-bill.
//
// A zero watermark (no fire this window) makes every method a no-op:
// exists() false, deferredBucket() false — closers behave exactly as before.
type thresholdWatermark struct {
	billedThrough *time.Time
	lines         []domain.InvoiceLineItem
}

// loadThresholdWatermark fetches the window's fire invoice + lines.
// ErrNotFound = no fire = zero watermark, nil error. Any other failure is
// loud: closing blind risks a double charge (feedback_no_silent_fallbacks).
func (e *Engine) loadThresholdWatermark(ctx context.Context, tenantID, subID string, periodStart, periodEnd time.Time) (thresholdWatermark, error) {
	return e.loadThresholdWatermarkTx(ctx, nil, tenantID, subID, periodStart, periodEnd)
}

// loadThresholdWatermarkTx is loadThresholdWatermark with the fire invoice read
// on tx (nil: its own connection). A closer that holds the subscription row
// lock reads on its tx, after the lock: a fire commits under the same lock, so
// every fire committed before it is seen (BillOnPlanSwapTx, the immediate
// cancel). The read holds the fire invoice FOR KEY SHARE until tx ends, so a
// void of it waits for the closer's commit.
func (e *Engine) loadThresholdWatermarkTx(ctx context.Context, tx *sql.Tx, tenantID, subID string, periodStart, periodEnd time.Time) (thresholdWatermark, error) {
	var wmInv domain.Invoice
	var err error
	if tx != nil {
		wmInv, err = e.invoices.GetLatestThresholdInvoiceForCycleTx(ctx, tx, tenantID, subID, periodStart, periodEnd)
	} else {
		wmInv, err = e.invoices.GetLatestThresholdInvoiceForCycle(ctx, tenantID, subID, periodStart, periodEnd)
	}
	if errors.Is(err, errs.ErrNotFound) {
		return thresholdWatermark{}, nil
	}
	if err != nil {
		return thresholdWatermark{}, fmt.Errorf("lookup threshold invoice for cycle: %w", err)
	}
	lines, err := e.invoices.ListLineItems(ctx, tenantID, wmInv.ID)
	if err != nil {
		return thresholdWatermark{}, fmt.Errorf("list watermark invoice lines: %w", err)
	}
	end := wmInv.BillingPeriodEnd
	return thresholdWatermark{billedThrough: &end, lines: lines}, nil
}

func (w thresholdWatermark) exists() bool { return w.billedThrough != nil }

// billedBase reports whether the fire invoice carries a base fee line. A
// fire no longer bills a base (the period's own invoice does), but one made
// before that change billed the full in_arrears base, and the period's
// closer must not bill it again. Keyed on the invoice's lines, like the
// non-additive exemption, so an open period that crosses the upgrade bills
// its base exactly once.
func (w thresholdWatermark) billedBase() bool {
	for _, l := range w.lines {
		if l.LineType == domain.LineTypeBaseFee {
			return true
		}
	}
	return false
}

// ThresholdBilledThroughTx implements subscription.Biller for the atomic
// plan swap (ADR-115, fire-vs-swap): on the swap's tx, after its period CAS
// took the subscription row lock, the instant a threshold fire already billed
// the sub's CURRENT period through — nil when none fired. A written-off fire
// still billed it; a voided one did not (the watermark predicate). The swap
// refuses to re-anchor the period below that instant; see
// applyCrossIntervalPlanSwapTx.
func (e *Engine) ThresholdBilledThroughTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription) (*time.Time, error) {
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return nil, nil
	}
	wm, err := e.invoices.GetLatestThresholdInvoiceForCycleTx(ctx, tx, sub.TenantID, sub.ID, *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd)
	if errors.Is(err, errs.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup threshold invoice for cycle: %w", err)
	}
	end := wm.BillingPeriodEnd
	return &end, nil
}

// bucketOnFire reports whether the fire billed this (meter, rating rule
// version) bucket.
func (w thresholdWatermark) bucketOnFire(meterID, ratingRuleVersionID string) bool {
	for _, li := range w.lines {
		if li.LineType == domain.LineTypeUsage && li.MeterID == meterID && li.RatingRuleVersionID == ratingRuleVersionID {
			return true
		}
	}
	return false
}

// deferredBucket reports whether the fire deliberately DEFERRED this
// non-additive bucket (dropped its line) — the close must
// bill it over the FULL window, exactly once.
func (w thresholdWatermark) deferredBucket(aggregation, meterID, ratingRuleVersionID string) bool {
	return w.exists() && nonAdditive(aggregation) && !w.bucketOnFire(meterID, ratingRuleVersionID)
}

// noteThresholdDeferred emits the loudness floor for a crossed-but-deferred
// cap (ADR-066 §4b): one audit/timeline row + one WARN per (sub, period) —
// not tick-spam (the scan re-evaluates a pure-max/last crossed sub every tick
// for the rest of the cycle, since no invoice exists for the probe to find).
// Dedup is in-memory: a process restart re-emits once per survivor, which is
// acceptable for an operator-visibility artifact (a duplicate timeline row
// beats a missed one; no new table for a dedup ledger).
func (e *Engine) noteThresholdDeferred(ctx context.Context, sub domain.Subscription, eval thresholdEval, periodStart time.Time) {
	key := sub.ID + "|" + periodStart.UTC().Format(time.RFC3339)
	if _, already := e.deferredThresholdNotes.LoadOrStore(key, struct{}{}); already {
		return
	}
	slog.Warn("threshold crossed but every billable line is a non-additive bucket — deferring to cycle close",
		"subscription_id", sub.ID,
		"tenant_id", sub.TenantID,
		"running_subtotal", eval.RunningSubtotal,
		"amount_gte", sub.BillingThresholds.AmountGTE,
	)
	if e.auditLogger != nil {
		_ = e.auditLogger.Log(ctx, sub.TenantID, domain.AuditActionSubscriptionThresholdDeferred, "subscription", sub.ID, sub.Code, map[string]any{
			"amount_gte":       sub.BillingThresholds.AmountGTE,
			"running_subtotal": eval.RunningSubtotal,
			"reason":           "max_last_meters_bill_at_cycle_close",
		})
	}
}

// healStrandedZeroDue repairs the ADR-066 crash window: an invoice committed
// as finalized with nothing owed (born $0, or fully credited) whose MarkPaid
// never ran because the process died between the create and the mark. Both
// re-entry paths call it — the threshold fire-once probe and billOnePeriod's
// ErrAlreadyExists branch — since neither ever reaches the normal MarkPaid
// gate again. Idempotent by construction (the gate re-checks live state;
// MarkPaid's already-paid branch is a no-op) and best-effort at the probe
// call site: a failure WARNs and the same probe re-enters next tick.
// The paidAt argument is the caller's effective now (test-clock aware) so a
// healed simulated invoice's paid_at stays in the simulation's time domain.
func (e *Engine) healStrandedZeroDue(ctx context.Context, tenantID string, inv domain.Invoice, paidAt time.Time) {
	if inv.Status != domain.InvoiceFinalized ||
		inv.PaymentStatus != domain.PaymentPending ||
		inv.AmountDueCents > 0 {
		return
	}
	if _, err := e.invoices.MarkPaid(ctx, tenantID, inv.ID, "", paidAt); err != nil {
		slog.Warn("heal stranded zero-due invoice: mark paid failed (will retry on next re-entry)",
			"invoice_id", inv.ID, "tenant_id", tenantID, "error", err)
		return
	}
	slog.Info("healed stranded zero-due invoice (crash between create and mark-paid)",
		"invoice_id", inv.ID, "tenant_id", tenantID)
}

// evaluateThresholds computes the partial-cycle running totals for a sub and
// reports whether any configured cap has been crossed. Reuses
// previewWithWindow's per-item base-fee + per-(meter, rule) usage line
// computation so the scan and the cycle scan agree on what "running total"
// means — fed to the same priority+claim LATERAL JOIN as the natural cycle.
func (e *Engine) evaluateThresholds(ctx context.Context, sub domain.Subscription, periodStart, now time.Time) (thresholdEval, error) {
	preview, err := e.previewWithWindow(ctx, sub, periodStart, now)
	if err != nil {
		return thresholdEval{}, err
	}

	// A threshold counts and bills usage only. Base fees stay with the
	// period's own invoice: an in_advance base is prepaid by BillOnCreate or
	// the previous close, and an in_arrears base is billed by the period's
	// close or immediate cancel, prorated as it would be with no fire. The
	// fire used to bill the full in_arrears base, so a cancel after it
	// charged the whole period's base for part of it.

	// Roll up TWO totals across the previewed lines (ADR-066 §4 subtotal
	// split). capRunning is the amount_gte comparison + event payload — the
	// customer's committed usage spend. billedSubtotal is the sum of lines that
	// actually ride the invoice — it feeds tax and the invoice header. They
	// diverge exactly when a non-additive bucket is dropped; collapsing them
	// charged the customer for
	// invisible lines (header > sum of rendered lines). Multi-currency subs
	// already filtered upstream at PATCH time, so summing across lines is
	// safe — there's only one currency.
	var capRunning, billedSubtotal int64
	currency := ""
	lineItems := make([]domain.InvoiceLineItem, 0, len(preview.Lines))
	for _, pl := range preview.Lines {
		if pl.LineType == "base_fee" {
			// Not billed here, but its currency (the plan's) still names the
			// invoice's, as when the base rode the fire.
			if currency == "" {
				currency = pl.Currency
			}
			continue
		}
		// Non-additive buckets (a max or last meter's) cannot be
		// split across a threshold fire and a cycle close: max[0,t1) +
		// max[t1,end) ≥ max[0,end). ADR-066 §4: DROP the line from the
		// invoice — the cycle close bills the bucket full-period exactly once
		// (the close-side clamp exemption). The amount still counts toward
		// the amount_gte cap (committed spend — for max-metered GPU
		// concurrency it can be MOST of the spend; the fire-once probe bounds
		// this to one fire per cycle, so counting it cannot loop).
		//
		// Every bucket of a meter bills under the meter's one aggregation
		// (ADR-130 §2), which the rater puts on the line.
		capRunning += pl.AmountCents
		if pl.LineType == "usage" && nonAdditive(pl.Aggregation) {
			continue
		}
		billedSubtotal += pl.AmountCents
		if currency == "" {
			currency = pl.Currency
		}
		// A usage line's window is the one its meter was rated over: from
		// the meter's usage floor (trial end, another subscription's
		// coverage end), not always the period start (ADR-012 amendment
		// 2026-10-22).
		var ps, pe *time.Time
		if pl.LineType == "usage" {
			s := preview.floors.start(pl.MeterID, periodStart, now)
			n := now
			ps = &s
			pe = &n
		}
		lineType := domain.InvoiceLineItemType(pl.LineType)
		quantity := pl.Quantity.IntPart()
		// Usage lines carry the exact fractional quantity (cycle-writer
		// parity, engine.go billOnePeriod) so qty × unit reconciles with
		// the amount on display surfaces. Base-fee lines keep the zero
		// value per the QuantityDecimal contract ("use Quantity").
		var qtyDecimal decimal.Decimal
		if lineType == domain.LineTypeUsage {
			qtyDecimal = pl.Quantity
		}
		lineItems = append(lineItems, domain.InvoiceLineItem{
			LineType:            lineType,
			MeterID:             pl.MeterID,
			MeterUnit:           pl.MeterUnit,
			Description:         pl.Description,
			Quantity:            quantity,
			QuantityDecimal:     qtyDecimal,
			UnitAmountCents:     pl.UnitAmountCents,
			AmountCents:         pl.AmountCents,
			TotalAmountCents:    pl.AmountCents,
			Currency:            pl.Currency,
			PricingMode:         pl.PricingMode,
			RatingRuleVersionID: pl.RatingRuleVersionID,
			// Copy the stamped nominal rate through from the preview line so
			// overage usage lines show the clean configured rate like cycle
			// usage lines (nil for non-flat / base_fee — falls back to
			// effective). ADR-054 re-examination.
			NominalUnitAmountDecimal: pl.NominalUnitAmountDecimal,
			BillingPeriodStart:       ps,
			BillingPeriodEnd:         pe,
		})
	}

	eval := thresholdEval{
		RunningSubtotal: capRunning,
		BilledSubtotal:  billedSubtotal,
		LineItems:       lineItems,
		InvoiceCurrency: currency,
	}
	for _, u := range preview.leftOut {
		if nonAdditive(u.Aggregation) {
			continue
		}
		if u.BelowZero {
			eval.BelowZero = append(eval.BelowZero, u)
		} else {
			eval.Unpriced = append(eval.Unpriced, u)
		}
	}

	// Amount cap: committed running spend vs the configured cap.
	bt := sub.BillingThresholds
	if bt.AmountGTE > 0 && capRunning >= bt.AmountGTE {
		eval.CrossedAny = true
		eval.CrossedAmount = true
	}

	// Per-item caps: sum the running quantity across each item's plan
	// meters during [floor, now), the window the fire bills each meter over
	// (the preview's usage floors). The meter quantity is the same
	// figure the natural cycle would bill — pulled from the priority+claim
	// resolver via AggregateClaims. Item-to-meter is via the plan
	// (item.plan_id -> plan.meter_ids). Cross-currency items already
	// rejected upstream, so summing across plans is safe.
	if !eval.CrossedAny && len(bt.ItemThresholds) > 0 {
		// Build a map from subscription_item_id -> running quantity sum.
		// The sum aggregates every meter on the item's plan.
		itemRunning := make(map[string]decimal.Decimal, len(sub.Items))
		for _, it := range sub.Items {
			// Read errors fail this subscription's evaluation (it retries
			// next tick; other subscriptions are unaffected). Skipping the
			// item, a meter, or a meter's usage under-counted the running
			// total, so a crossed item threshold silently did not fire.
			plan, err := e.pricing.GetPlan(ctx, sub.TenantID, it.PlanID)
			if err != nil {
				return thresholdEval{}, fmt.Errorf("item threshold: get plan %s for item %s: %w", it.PlanID, it.ID, err)
			}
			total := decimal.Zero
			for _, meterID := range plan.MeterIDs {
				meter, mErr := e.pricing.GetMeter(ctx, sub.TenantID, meterID)
				if mErr != nil {
					return thresholdEval{}, fmt.Errorf("item threshold: get meter %s: %w", meterID, mErr)
				}
				from := preview.floors.start(meterID, periodStart, now)
				if !from.Before(now) {
					continue
				}
				rules, rErr := e.pricing.ListMeterPricingRulesByMeter(ctx, sub.TenantID, meterID)
				if rErr != nil {
					return thresholdEval{}, fmt.Errorf("item threshold: list pricing rules for meter %s: %w", meterID, rErr)
				}
				aggs, aErr := e.usage.AggregateClaims(ctx, sub.TenantID, sub.CustomerID, meterID, meter.Aggregation, domain.ClaimsOfRules(rules), from, now)
				if aErr != nil {
					return thresholdEval{}, fmt.Errorf("item threshold: aggregate meter %s: %w", meterID, aErr)
				}
				for _, agg := range aggs {
					total = total.Add(agg.Quantity)
				}
			}
			itemRunning[it.ID] = total
		}
		for _, t := range bt.ItemThresholds {
			running, ok := itemRunning[t.SubscriptionItemID]
			if !ok {
				continue
			}
			if running.GreaterThanOrEqual(t.UsageGTE) {
				eval.CrossedAny = true
				eval.CrossedItem = true
				eval.CrossedItemID = t.SubscriptionItemID
				break
			}
		}
	}

	return eval, nil
}

// fireThreshold emits the early-finalize invoice. The subscription's period
// does not move: its close bills the rest of the period. Returns (true, nil)
// on a successful fire, (false, nil) when a concurrent retry already produced
// the same invoice or the period moved under it (idempotent skip).
//
// The invoice is built from the same line items evaluateThresholds already
// computed — re-aggregating would risk a different running total if usage
// landed between evaluate and fire. Tax + credit are applied via
// the same paths the natural cycle uses so an early-finalize charge looks
// identical to the customer.
func (e *Engine) fireThreshold(ctx context.Context, sub domain.Subscription, eval thresholdEval, periodStart, now time.Time) (bool, error) {
	if e.settings == nil {
		return false, fmt.Errorf("settings reader required for invoice numbering")
	}
	// Resolved before tax is computed and before the fire tx opens: a read
	// failure fails the fire with nothing written, and the next tick retries.
	netDays, err := e.netPaymentTermDays(ctx, sub.TenantID)
	if err != nil {
		return false, err
	}
	billingLoc, err := e.tenantLocation(ctx, sub.TenantID)
	if err != nil {
		return false, err
	}

	invoiceCurrency := eval.InvoiceCurrency
	if invoiceCurrency == "" {
		// Empty preview lines (no usage) would land here.
		// In that case the running subtotal is zero so we shouldn't have
		// reached the fire path — defensive bail.
		return false, fmt.Errorf("no invoice currency resolved for threshold fire")
	}
	invoiceCurrency, err = e.invoiceCurrency(ctx, sub, invoiceCurrency)
	if err != nil {
		return false, err
	}

	// Coupons removed 2026-05-29 (Phase A1). Discount stays at zero;
	// discount intent flows through the credit ledger.
	//
	// BilledSubtotal, NOT RunningSubtotal: the invoice charges the sum of its
	// rendered lines. RunningSubtotal may exceed it by dropped non-additive
	// buckets (ADR-066 §4) — using it here charged the customer for invisible
	// lines and double-billed them again at cycle close.
	subtotal := eval.BilledSubtotal
	var discountCents int64

	// Propagate tax errors rather than discarding them: a swallowed error
	// yields a zero-value TaxApplication (TaxStatus="") that finalizes a $0
	// threshold invoice and permanently consumes the cycle idempotency slot,
	// leaving the over-threshold usage unbilled. Mirrors the cycle-close and
	// proration call sites. (audit: velox-ops threshold tax-discard finding)
	taxApp, err := e.ApplyTaxToLineItems(ctx, sub.TenantID, sub.CustomerID, invoiceCurrency, subtotal, discountCents, eval.LineItems)
	if err != nil {
		return false, fmt.Errorf("apply tax: %w", err)
	}
	totalWithTax := taxApp.SubtotalCents - taxApp.DiscountCents + taxApp.TaxAmountCents

	dueAt := domain.AddDaysIn(now, netDays, billingLoc)

	// Shared finalization gate (tax-pending OR pause-collection → Draft),
	// same as every other invoice writer. The scan currently skips paused
	// subs upstream (scanOneThreshold), so the pause arm is unreachable
	// today — but a hand-rolled tax-only gate here was wrong-by-
	// construction: any relaxation of that skip would have finalized and
	// auto-charged a paused subscription.
	invStatus := domain.InvoiceFinalizationStatus(taxApp.TaxStatus, sub.PauseCollection, now)

	// Emit the threshold invoice. The partial unique index on
	// (tenant, sub, billing_period_start) WHERE billing_reason='threshold'
	// is the schema-level idempotency seam — a re-tick lands on
	// errs.ErrAlreadyExists and we short-circuit. Note: we use periodStart as
	// the boundary key, not now, so two ticks fired against the same
	// in-flight cycle dedup to the same row even though their wall-clock
	// differs. The invoice number is allocated inside the fire tx, after the
	// row CAS (ADR-115), so a lost race burns none.
	invoiceRow := domain.Invoice{
		CustomerID:      sub.CustomerID,
		SubscriptionID:  sub.ID,
		BillingTimezone: billingLoc.String(),
		Status:          invStatus,
		PaymentStatus:   domain.PaymentPending,
		Currency:        invoiceCurrency,
		SubtotalCents:   taxApp.SubtotalCents,
		DiscountCents:   taxApp.DiscountCents,
		// ONE assignment carries every tax fact (see domain/tax_facts.go) —
		// this writer once dropped TaxErrorCode by hand-copy; now impossible.
		TaxFacts:           taxApp.TaxFacts,
		TotalAmountCents:   totalWithTax,
		AmountDueCents:     totalWithTax,
		BillingPeriodStart: periodStart,
		BillingPeriodEnd:   now,
		IssuedAt:           &now,
		DueAt:              &dueAt,
		// CreatedAt on test-clock time — keeps engine-driven rows
		// internally consistent (Stripe / Lago / Orb pattern).
		CreatedAt:          now,
		NetPaymentTermDays: netDays,
		BillingReason:      domain.BillingReasonThreshold,
		// Threshold invoices fire on clock-pinned subs too (the
		// ScanThresholdsForClock catchup path); without this stamp the
		// invoice landed is_simulated=false and rendered without the
		// Simulated badge while sibling cycle/proration invoices on the
		// same clock showed it. Matches every other engine writer.
		IsSimulated: sub.TestClockID != "",
	}

	// ONE transaction for every fire (ADR-115, extending ADR-066): the period
	// CAS is its FIRST statement, then the invoice number, the invoice + lines
	// and the finalize audit row.
	//
	// The CAS proves the snapshot the scan page read — status=active, the
	// period start and the watermark, exactly what scanOneThreshold gated on —
	// under the subscription row lock. A cycle close (leader tick or operator
	// POST /v1/billing/run), a plan swap, a cancel or a sibling fire that
	// committed since makes it 0 rows: the fire rolls back with nothing
	// written and the next tick re-evaluates against the new period. Pre-fix
	// the fire never touched the row, so a fire in flight across an operator
	// close billed [periodStart, now) on both invoices and could rewind the
	// watermark.
	//
	// The CAS writes the row VERIFY-ONLY: the same values, so the period does
	// not move, but the UPDATE takes the lock and proves the snapshot through
	// the one statement every closer shares. (This bumps updated_at; nothing
	// outside the package reads it.)
	//
	// External calls (CommitTax, charge) stay post-commit — the tx never
	// spans network I/O.
	expected := subscription.SnapshotOf(sub)
	belowZero := netNegativeUsage{}
	for _, b := range eval.BelowZero {
		belowZero.add(b.MeterID, b.EventName, b.MeterName, b.Quantity, periodStart, now) // the window the fire priced
	}
	var inv domain.Invoice
	err = e.subs.WithTenantTx(ctx, sub.TenantID, func(tx *sql.Tx) error {
		if err := e.subs.ClosePeriodTx(ctx, tx, sub.TenantID, sub.ID, expected,
			*sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd, *sub.NextBillingAt, sub.BillingAnchorDay); err != nil {
			return err
		}
		num, err := e.settings.NextInvoiceNumberTx(ctx, tx, sub.TenantID)
		if err != nil {
			return fmt.Errorf("allocate invoice number: %w", err)
		}
		invoiceRow.InvoiceNumber = num
		created, err := e.invoices.CreateInvoiceWithLineItemsTx(ctx, tx, sub.TenantID, invoiceRow, eval.LineItems)
		if err != nil {
			return err
		}
		inv = created
		// The finalize audit row rides the fire tx (ADR-090 shared fate).
		if err := e.emitFinalizeAuditTx(ctx, tx, created); err != nil {
			return err
		}
		// So does the row for usage below zero this invoice counts as 0.
		return e.noteNetNegativeTx(ctx, tx, sub, belowZero, domain.BillingReasonThreshold, periodStart, now, created)
	})
	if err != nil {
		if errors.Is(err, errs.ErrAlreadyExists) {
			slog.Info("threshold invoice already exists for cycle (idempotent skip)",
				"subscription_id", sub.ID,
				"period_start", periodStart,
			)
			return false, nil
		}
		if errors.Is(err, subscription.ErrWatermarkMoved) {
			slog.InfoContext(ctx, "threshold fire skipped — period moved under the fire; re-evaluated next tick against the new period",
				"subscription_id", sub.ID, "tenant_id", sub.TenantID,
				"period_start", periodStart)
			return false, nil
		}
		return false, fmt.Errorf("create threshold invoice: %w", err)
	}

	// The finalize AUDIT row already committed inside the fire tx above (same
	// finalize-row contract as the cycle path). Here
	// only the post-commit WEBHOOK fires.
	e.dispatchInvoiceFinalized(ctx, sub, inv)

	// Commit tax (Stripe parity) — matches the cycle scan's flow.
	if inv.TaxProvider != "" && inv.TaxCalculationID != "" {
		if err := e.CommitTax(ctx, sub.TenantID, inv.ID, inv.TaxCalculationID); err != nil {
			slog.Warn("threshold scan: tax commit failed after invoice creation",
				"error", err, "tenant_id", sub.TenantID, "invoice_id", inv.ID)
		}
	}

	// Collect now, through the one collector (same as cycle close): apply
	// credits, settle if nothing is owed, else charge the card or email for
	// one. The fire tx already queued the invoice, so a crash here leaves it
	// for the next sweep. A tax-pending threshold invoice is a draft and is
	// collected when the tax-retry chain finalizes it.
	e.CollectInvoice(ctx, sub.TenantID, inv.ID, &now)

	// The event carries the subscription as the fire left it (ADR-131): the
	// scan's snapshot predates the fire's write, so it is read again.
	if e.events != nil {
		fired, err := e.subs.Get(ctx, sub.TenantID, sub.ID)
		if err != nil {
			slog.Error("threshold scan: subscription.threshold_crossed not sent: the subscription could not be read after the fire",
				"subscription_id", sub.ID, "tenant_id", sub.TenantID, "error", err)
		} else {
			payload := domain.SubscriptionWebhookData(fired, map[string]any{
				"invoice_id":       inv.ID,
				"running_subtotal": eval.RunningSubtotal,
				"crossed_amount":   eval.CrossedAmount,
				"crossed_item_id":  eval.CrossedItemID,
			})
			if err := e.events.Dispatch(ctx, sub.TenantID, domain.EventSubscriptionThresholdCrossed, payload); err != nil {
				slog.Error("threshold scan: dispatch subscription.threshold_crossed failed",
					"subscription_id", sub.ID, "tenant_id", sub.TenantID, "error", err)
			}
		}
	}
	// Audit row so the crossing shows on the subscription activity feed —
	// pre-fix only the webhook fired and the invoice appeared, with no
	// timeline row explaining WHY an invoice landed mid-cycle.
	if e.auditLogger != nil {
		meta := map[string]any{
			"invoice_id":     inv.ID,
			"invoice_number": inv.InvoiceNumber,
			"amount_cents":   inv.TotalAmountCents,
			"currency":       inv.Currency,
		}
		_ = e.auditLogger.Log(ctx, sub.TenantID, domain.AuditActionSubscriptionThresholdCrossed, "subscription", sub.ID, sub.Code, meta)
	}

	slog.Info("threshold-fired invoice generated",
		"invoice_id", inv.ID,
		"subscription_id", sub.ID,
		"running_subtotal", eval.RunningSubtotal,
		"crossed_amount", eval.CrossedAmount,
		"crossed_item_id", eval.CrossedItemID,
		"total_cents", totalWithTax,
	)

	return true, nil
}
