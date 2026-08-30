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
	"github.com/sagarsuperuser/velox/internal/platform/money"
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
	// committed spend, including (iff reset=false) non-additive buckets that
	// bill at cycle close instead of riding this invoice. Feeds the cap check
	// and the threshold_crossed event payload ONLY.
	RunningSubtotal int64
	// BilledSubtotal is the sum of LineItems' amounts — what the invoice
	// actually charges. Feeds tax + the invoice header. Diverges from
	// RunningSubtotal exactly when a non-additive bucket is dropped or
	// cap-excluded (ADR-066 §4).
	BilledSubtotal  int64
	LineItems       []domain.InvoiceLineItem
	InvoiceCurrency string
}

// nonAdditiveMode reports whether a rule bucket's aggregation cannot be split
// across a threshold fire and a cycle close (max[0,t1) + max[t1,end) ≥
// max[0,end); last is a point-in-time read). last_ever additionally ignores
// window bounds entirely. ADR-066 §4.
func nonAdditiveMode(m domain.AggregationMode) bool {
	switch m {
	case domain.AggMax, domain.AggLastDuringPeriod, domain.AggLastEver:
		return true
	}
	return false
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
	// pause_collection neuters the financial side; firing a threshold under
	// pause would emit a draft that can't be charged or dunned. Skip — the
	// scan resumes when collection does.
	if sub.PauseCollection != nil {
		return false, nil
	}

	now := e.effectiveNow(ctx, sub)
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

	// Fire-once probe: if a threshold invoice already exists for this cycle,
	// this sub is done until the period rolls (reset=false) or the cycle
	// re-anchors (reset=true — which moves periodStart, emptying this window).
	// Pre-fix the scan re-evaluated and re-fired every tick after a reset=false
	// crossing: each re-fire burned an invoice number + a paid Stripe Tax
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
	// The probe fetches the full row (not just the watermark instant) so a
	// hit can heal the $0/credited MarkPaid crash window: a crash between
	// invoice-create and MarkPaid re-enters HERE on every later tick — the
	// only re-entry this sub gets — so this is where the stranded
	// payment_pending row is repaired (ADR-066).
	if existing, err := e.invoices.GetLatestThresholdInvoiceForCycle(ctx, sub.TenantID, sub.ID, periodStart, *sub.CurrentBillingPeriodEnd); err == nil {
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
	// line was a dropped non-additive bucket (pure-max/last sub under
	// reset=false). There is nothing to invoice — the cycle close bills those
	// buckets full-period. Skip BEFORE fireThreshold so no invoice number is
	// minted and no paid tax calculation runs (the pre-fix currency bail sat
	// after both, erroring every tick), with a once-per-(sub, period)
	// operator artifact so the crossing isn't silent: the Spend-thresholds
	// card says a cap exists; the timeline must say why no invoice appeared.
	if len(eval.LineItems) == 0 {
		e.noteThresholdDeferred(ctx, sub, eval, periodStart)
		return false, nil
	}

	return e.fireThreshold(ctx, sub, eval, periodStart, now)
}

// thresholdWatermark is one close window's threshold-fire ground truth
// (ADR-066 §4) — shared by BOTH period-closers (billOnePeriod and
// billFinalOnImmediateCancelImpl) so the protocol cannot drift between them:
//
//   - billedThrough: usage before this instant (and the in_arrears base)
//     already billed on the mid-cycle threshold invoice; additive buckets
//     bill only the residual window.
//   - lines: the fire invoice's persisted line items — the per-bucket
//     ground-truth for the non-additive clamp exemption. A max/last bucket
//     with NO line was deliberately deferred by the fire and must bill
//     full-window at close; one WITH a line already billed its window.
//     Keyed on the invoice, never the mutable reset_billing_cycle config —
//     an operator PATCH between fire and close would otherwise resurrect
//     the billed-by-nobody gap or a double-bill.
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
	wmInv, err := e.invoices.GetLatestThresholdInvoiceForCycle(ctx, tenantID, subID, periodStart, periodEnd)
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

// ThresholdBilledThroughTx implements subscription.Biller for the atomic
// plan swap (ADR-115, fire-vs-swap): on the swap's tx, after its period CAS
// took the subscription row lock, the instant a threshold fire already billed
// the sub's CURRENT period through — nil when none fired. The swap refuses to
// re-anchor the period below that instant; see applyCrossIntervalPlanSwapTx.
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
// non-additive bucket (dropped its line under reset=false) — the close must
// bill it over the FULL window, exactly once.
func (w thresholdWatermark) deferredBucket(mode domain.AggregationMode, meterID, ratingRuleVersionID string) bool {
	return w.exists() && nonAdditiveMode(mode) && !w.bucketOnFire(meterID, ratingRuleVersionID)
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
		_ = e.auditLogger.Log(ctx, sub.TenantID, "subscription.threshold_deferred", "subscription", sub.ID, sub.Code, map[string]any{
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

	// in_advance base fees are billed up front by BillOnCreate / cycle close,
	// so they must NOT count toward the threshold running total or ride along
	// on the early-finalize invoice — doing so double-bills the prepaid base.
	// Each base_fee preview line carries its own PlanID + BaseBillTiming
	// (stamped at emission, json:"-"), so the skip/proration below matches
	// PER LINE — the previous parallel-array rebuild of the preview's
	// emission order silently mis-attributed plans if that order ever
	// changed (2026-07-10 design review: positional contract between a
	// display surface and a money writer).

	// Roll up TWO totals across the previewed lines (ADR-066 §4 subtotal
	// split). capRunning is the amount_gte comparison + event payload — the
	// customer's committed spend. billedSubtotal is the sum of lines that
	// actually ride the invoice — it feeds tax and the invoice header. They
	// diverge exactly when a non-additive bucket is dropped (reset=false) or
	// cap-excluded (reset=true); collapsing them charged the customer for
	// invisible lines (header > sum of rendered lines). Multi-currency subs
	// already filtered upstream at PATCH time, so summing across lines is
	// safe — there's only one currency.
	var capRunning, billedSubtotal int64
	reset := sub.BillingThresholds != nil && sub.BillingThresholds.ResetBillingCycle
	currency := ""
	lineItems := make([]domain.InvoiceLineItem, 0, len(preview.Lines))
	for _, pl := range preview.Lines {
		if pl.LineType == "base_fee" {
			if pl.PlanID == "" {
				// Every base_fee line is stamped at emission; an unstamped
				// line means a new producer bypassed the contract — fail
				// loud rather than guess whose base this is (money path).
				return thresholdEval{}, fmt.Errorf("threshold fire: base_fee preview line %q has no plan stamp", pl.Description)
			}
			if pl.BaseBillTiming == domain.BillInAdvance {
				// Already prepaid — skip from running total and line items.
				continue
			}
			// reset_billing_cycle=true: the fire re-anchors the cycle, so the
			// cycle close that would normally true-up the base never bills this
			// window — the threshold invoice IS the window's base bill. Prorate
			// it to the elapsed fraction; pre-fix the full month's base rode
			// every fire, so a sub crossing 3x/month paid base 3x. The formula
			// mirrors emitBaseSegmentLine EXACTLY: denominator = the plan's own
			// FULL interval advanced from periodStart (domain/subscription.go
			// invariant — never the current period length, which over-bills
			// re-anchored stubs, mid-cycle-created periods, and cross-interval
			// items, and can be zero for a sub-day stub). The whole line triple
			// is rewritten (description/unit/amount) so qty × unit == amount on
			// every render surface. reset=false is untouched: the cycle close
			// bills only the post-fire residual and skips base when a threshold
			// watermark exists, so the full base here stays correct.
			if sub.BillingThresholds != nil && sub.BillingThresholds.ResetBillingCycle {
				plan, err := e.pricing.GetPlan(ctx, sub.TenantID, pl.PlanID)
				if err != nil {
					return thresholdEval{}, fmt.Errorf("get plan %s for base proration: %w", pl.PlanID, err)
				}
				qty := pl.Quantity.IntPart()
				loc := e.tenantLocation(ctx, sub.TenantID)
				// Anchor derived from THE PERIOD BEING BILLED, never the
				// sub's live billing_anchor_day — a cross-interval swap
				// rewrites the sub anchor before this window closes and
				// the mutated anchor stretches the denominator (the #581
				// "15/46 days" leak; that fix covered emitBaseSegmentLine's
				// four billers but missed this fifth copy of the math).
				anchorDay := domain.AnchorDayFor(periodStart, sub.BillingTime, plan.BillingInterval, loc)
				fullCycleDays := roundDays(advanceBillingPeriod(periodStart, plan.BillingInterval, loc, anchorDay).Sub(periodStart))
				segDays := roundDays(now.Sub(periodStart))
				if fullCycleDays > 0 && segDays < fullCycleDays {
					prorated := money.RoundHalfToEven(plan.BaseAmountCents*qty*int64(segDays), int64(fullCycleDays))
					pl.AmountCents = prorated
					pl.Description = fmt.Sprintf("%s - base fee (qty %d, prorated %d/%d days)", plan.Name, qty, segDays, fullCycleDays)
					if qty > 0 {
						pl.UnitAmountCents = money.RoundHalfToEven(prorated, qty)
					}
				}
			}
		}
		// Non-additive buckets (max / last_during_period / last_ever) cannot be
		// split across a threshold fire and a cycle close: max[0,t1) +
		// max[t1,end) ≥ max[0,end). ADR-066 §4:
		//
		//   - reset=false: DROP the line from the invoice — the cycle close
		//     bills the bucket full-period exactly once (the close-side clamp
		//     exemption). The amount still counts toward the amount_gte cap
		//     (committed spend — for max-metered GPU concurrency it can be
		//     MOST of the spend; the fire-once probe bounds this to one fire
		//     per cycle, so counting it cannot loop).
		//   - reset=true: the line RIDES the invoice (the re-anchored stub
		//     never gets a close for this window — dropping would bill it by
		//     NOBODY), but the amount does NOT count toward the cap: a steady
		//     peak re-materializes in every re-anchored window, so counting it
		//     refires one invoice + card charge per scheduler tick.
		//
		// Classification is per RULE BUCKET (pl.AggregationMode rides from
		// AggregateByPricingRules through previewMeter), never per meter — one
		// meter can carry sum and max rules simultaneously.
		if pl.LineType == "usage" && nonAdditiveMode(pl.AggregationMode) {
			if !reset {
				capRunning += pl.AmountCents
				continue
			}
			billedSubtotal += pl.AmountCents
		} else {
			capRunning += pl.AmountCents
			billedSubtotal += pl.AmountCents
		}
		if currency == "" {
			currency = pl.Currency
		}
		var ps, pe *time.Time
		if pl.LineType == "usage" {
			s := periodStart
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

	// Amount cap: committed running spend vs the configured cap.
	bt := sub.BillingThresholds
	if bt.AmountGTE > 0 && capRunning >= bt.AmountGTE {
		eval.CrossedAny = true
		eval.CrossedAmount = true
	}

	// Per-item caps: sum the running quantity across each item's plan
	// meters during [periodStart, now). The meter quantity is the same
	// figure the natural cycle would bill — pulled from the priority+claim
	// resolver via AggregateByPricingRules. Item-to-meter is via the plan
	// (item.plan_id -> plan.meter_ids). Cross-currency items already
	// rejected upstream, so summing across plans is safe.
	if !eval.CrossedAny && len(bt.ItemThresholds) > 0 {
		// Build a map from subscription_item_id -> running quantity sum.
		// The sum aggregates every meter on the item's plan.
		itemRunning := make(map[string]decimal.Decimal, len(sub.Items))
		for _, it := range sub.Items {
			plan, err := e.pricing.GetPlan(ctx, sub.TenantID, it.PlanID)
			if err != nil {
				// Plan resolution errors are surfaced but don't kill the whole
				// scan — fall through, the per-item check just won't fire on
				// this row. The cycle scan will surface the same error later
				// if the misconfig persists.
				slog.Warn("threshold scan: get plan failed",
					"subscription_id", sub.ID,
					"item_id", it.ID,
					"plan_id", it.PlanID,
					"error", err,
				)
				continue
			}
			total := decimal.Zero
			for _, meterID := range plan.MeterIDs {
				meter, mErr := e.pricing.GetMeter(ctx, sub.TenantID, meterID)
				if mErr != nil {
					continue
				}
				defaultMode := mapMeterAggregation(meter.Aggregation)
				aggs, aErr := e.usage.AggregateByPricingRules(ctx, sub.TenantID, sub.CustomerID, meterID, defaultMode, periodStart, now)
				if aErr != nil {
					continue
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

// fireThreshold emits the early-finalize invoice and (when reset_billing_cycle
// is true) advances the subscription's cycle as if the period had ended
// naturally. Returns (true, nil) on a successful fire, (false, nil) when a
// concurrent retry already produced the same invoice (idempotent skip).
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

	// Resolve the items' plan map for cycle-advance interval.
	plans := make(map[string]domain.Plan, len(sub.Items))
	for _, it := range sub.Items {
		if _, ok := plans[it.PlanID]; ok {
			continue
		}
		pl, err := e.pricing.GetPlan(ctx, sub.TenantID, it.PlanID)
		if err != nil {
			return false, fmt.Errorf("get plan %s: %w", it.PlanID, err)
		}
		plans[it.PlanID] = pl
	}

	invoiceCurrency := eval.InvoiceCurrency
	if invoiceCurrency == "" {
		// Empty preview lines (zero base fees, zero usage) would land here.
		// In that case the running subtotal is zero so we shouldn't have
		// reached the fire path — defensive bail.
		return false, fmt.Errorf("no invoice currency resolved for threshold fire")
	}
	if e.profiles != nil {
		if bp, err := e.profiles.GetBillingProfile(ctx, sub.TenantID, sub.CustomerID); err == nil && bp.Currency != "" {
			invoiceCurrency = bp.Currency
		}
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

	netDays := 30
	if ts, err := e.settings.Get(ctx, sub.TenantID); err == nil && ts.NetPaymentTerms > 0 {
		netDays = ts.NetPaymentTerms
	}

	dueAt := now.AddDate(0, 0, netDays)

	// Shared finalization gate (tax-pending OR pause-collection → Draft),
	// same as every other invoice writer. The scan currently skips paused
	// subs upstream (scanOneThreshold), so the pause arm is unreachable
	// today — but a hand-rolled tax-only gate here was wrong-by-
	// construction: any relaxation of that skip would have finalized and
	// auto-charged a paused subscription.
	invStatus := domain.InvoiceFinalizationStatus(taxApp.TaxStatus, sub.PauseCollection)

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
		BillingTimezone: e.tenantLocation(ctx, sub.TenantID).String(),
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
	// the reset arm re-anchored WHERE id only and the reset=false arm never
	// touched the row, so a fire in flight across an operator close billed
	// [periodStart, now) on both invoices and could rewind the watermark.
	//
	//   - reset_billing_cycle=true re-anchors the cycle to `now` (recomputing
	//     the anchor day for the new cadence and routing through
	//     NextBillingPeriodEnd so a calendar sub re-snaps to the 1st, ADR-055).
	//     ADR-066: the insert and the re-anchor share fate — a crash or
	//     transient error between them once stranded the reset FOREVER
	//     (the fire-once probe found the committed invoice and skipped every
	//     later tick, silently degrading the sub to reset=false continuation
	//     and, under base proration, permanently under-billing base).
	//   - reset_billing_cycle=false writes the row VERIFY-ONLY: the same
	//     values, so the period does not move, but the UPDATE takes the lock
	//     and proves the snapshot through the one statement every closer
	//     shares. (This bumps updated_at; nothing outside the package reads it.)
	//
	// External calls (CommitTax, charge) stay post-commit — the tx never
	// spans network I/O.
	expected := subscription.SnapshotOf(sub)
	var inv domain.Invoice
	err = e.subs.WithTenantTx(ctx, sub.TenantID, func(tx *sql.Tx) error {
		if sub.BillingThresholds.ResetBillingCycle {
			loc := e.tenantLocation(ctx, sub.TenantID)
			interval := plans[sub.Items[0].PlanID].BillingInterval
			resetAnchorDay := domain.AnchorDayFor(now, sub.BillingTime, interval, loc)
			nextPeriodEnd := domain.NextBillingPeriodEnd(now, sub.BillingTime, interval, loc, resetAnchorDay)
			if err := e.subs.ClosePeriodTx(ctx, tx, sub.TenantID, sub.ID, expected, now, nextPeriodEnd, nextPeriodEnd, resetAnchorDay); err != nil {
				return err
			}
		} else if err := e.subs.ClosePeriodTx(ctx, tx, sub.TenantID, sub.ID, expected,
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
		return e.emitFinalizeAuditTx(ctx, tx, created)
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
	// finalize-row contract as the cycle path; feeds TTFI + the audit log). Here
	// only the post-commit WEBHOOK fires.
	e.dispatchInvoiceFinalized(ctx, sub, inv)

	// Commit tax (Stripe parity) — matches the cycle scan's flow.
	if inv.TaxProvider != "" && inv.TaxCalculationID != "" {
		if err := e.CommitTax(ctx, sub.TenantID, inv.ID, inv.TaxCalculationID); err != nil {
			slog.Warn("threshold scan: tax commit failed after invoice creation",
				"error", err, "tenant_id", sub.TenantID, "invoice_id", inv.ID)
		}
	}

	// Apply customer credits before charging. Same shape as the cycle scan —
	// INCLUDING the creditApplyOK gate (2026-05-30 fix, ported 2026-06-13): a
	// failed credit application must flag the invoice for the scheduler-retry
	// sweep and SKIP the inline charge below, otherwise the customer's card is
	// charged the FULL pre-credit total while their balance sits unconsumed.
	// The retry sweep re-applies credits before charging (processAutoCharge).
	creditApplyOK := true
	if e.credits != nil && totalWithTax > 0 {
		if _, err := e.credits.ApplyToInvoiceAt(ctx, sub.TenantID, sub.CustomerID, inv.ID, totalWithTax, now, inv.InvoiceNumber); err != nil {
			slog.Warn("threshold scan: failed to apply credits — flagging for retry; auto-charge skipped to avoid overcharge",
				"invoice_id", inv.ID, "error", err)
			creditApplyOK = false
			if err := e.invoices.SetAutoChargePending(ctx, sub.TenantID, inv.ID, true); err != nil {
				// A failed set(true) is a liveness sink: the invoice stays
				// invisible to RetryPendingCharges forever (playbook class G).
				slog.Warn("failed to queue invoice for charge retry", "invoice_id", inv.ID, "error", err)
			}
		}
	}

	// If nothing is owed — credits covered 100%, OR the invoice was born $0
	// (zero-priced usage lines crossing a usage_gte cap) — mark paid
	// immediately, BUT only on invoices that landed as finalized at create
	// time. Draft invoices (tax pending / pause-collection) stay draft with
	// credits applied. A tax-pending draft auto-finalizes later via the
	// tax-retry chain; a pause-collection draft stays draft until the operator
	// finalizes it. Mirrors billOnePeriod's gate (2026-05-22 fix — DEMO-000906).
	// The old `totalWithTax > 0` conjunct stranded $0 finalized invoices
	// payment_pending FOREVER — never charged (amount_due=0 skips the charge
	// arm), never paid, polluting the attention queue as overdue (ADR-066;
	// Stripe parity: zero-amount invoices are auto-marked paid, no payment
	// attempt).
	if creditApplyOK && inv.Status == domain.InvoiceFinalized {
		updatedInv, err := e.invoices.GetInvoice(ctx, sub.TenantID, inv.ID)
		if err == nil && updatedInv.AmountDueCents <= 0 {
			if _, err := e.invoices.MarkPaid(ctx, sub.TenantID, inv.ID, "", now); err != nil {
				slog.Warn("threshold scan: failed to mark fully-credited invoice as paid",
					"invoice_id", inv.ID, "error", err)
			} else {
				// Background credit settle — close any active dunning run so it
				// isn't left stale (best-effort; processRun pre-check backstops).
				e.resolveDunningRecovered(ctx, sub.TenantID, inv.ID)
			}
		}
	}

	// Auto-charge: synchronous with timeout, same behaviour as the cycle scan.
	//
	// The no-PM arm mirrors billOnePeriod's post-finalize block exactly.
	// Pre-fix the else was MISSING here: a customer with no payment method
	// crossing a spend threshold got a finalized invoice that was never
	// queued for charge-on-attach (auto_charge_pending stayed false, so
	// RetryPendingCharges never saw it — attaching a card later charged
	// nothing) and never notified — it sat payment_pending until it aged
	// into overdue. Cycle-close invoices took the correct arm; threshold
	// fires silently did nothing (2026-07-10 design-census finding).
	//
	// A tax-pending threshold invoice is a DRAFT (paused subs never reach
	// here — the scan skips them at entry) and is simply not collected here.
	// This site used to pre-set auto_charge_pending on that draft, because
	// the tax-retry chain finalized it without collecting and the flag was
	// the only thing that would make the sweep notice. That gap is closed at
	// its source now — invoice.Service.RetryTax queues the invoice when it
	// auto-finalizes — so pre-flagging would be a second owner of one
	// handoff, covering threshold fires but not cycle closes.
	if creditApplyOK && inv.AmountDueCents > 0 && inv.Status == domain.InvoiceFinalized {
		e.collectAfterFinalize(ctx, sub, inv, "threshold fire")
	}

	// Emit subscription.threshold_crossed before the optional cycle reset
	// so consumers see the crossing event ahead of any cycle-rollover side
	// effects.
	if e.events != nil {
		payload := map[string]any{
			"subscription_id":  sub.ID,
			"customer_id":      sub.CustomerID,
			"invoice_id":       inv.ID,
			"running_subtotal": eval.RunningSubtotal,
			"crossed_amount":   eval.CrossedAmount,
			"crossed_item_id":  eval.CrossedItemID,
			"reset_cycle":      sub.BillingThresholds.ResetBillingCycle,
		}
		if err := e.events.Dispatch(ctx, sub.TenantID, domain.EventSubscriptionThresholdCrossed, payload); err != nil {
			slog.Error("threshold scan: dispatch subscription.threshold_crossed failed",
				"subscription_id", sub.ID, "tenant_id", sub.TenantID, "error", err)
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
		_ = e.auditLogger.Log(ctx, sub.TenantID, "subscription.threshold_crossed", "subscription", sub.ID, sub.Code, meta)
	}

	// NOTE: the reset=true cycle re-anchor already committed atomically with
	// the invoice insert above — there is deliberately no post-hoc period
	// write here (the old two-write shape with its "let the next tick
	// reconcile" comment was a lie: the fire-once probe blocked every retry).

	slog.Info("threshold-fired invoice generated",
		"invoice_id", inv.ID,
		"subscription_id", sub.ID,
		"running_subtotal", eval.RunningSubtotal,
		"crossed_amount", eval.CrossedAmount,
		"crossed_item_id", eval.CrossedItemID,
		"total_cents", totalWithTax,
		"reset_cycle", sub.BillingThresholds.ResetBillingCycle,
	)

	return true, nil
}
