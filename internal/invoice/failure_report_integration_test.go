package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestFailureReport_MovesOnlyTheAttemptTheInvoiceWaitsOn runs every source
// of a failure report through the REAL store: the charge call reporting its
// own decline (ChargedAtSeq), a webhook or reconciler report, and a hosted
// Checkout attempt. An invoice records one attempt at a time; a report about
// any other attempt must leave the invoice — status, PaymentIntent, seq — as it
// was, send no notice, and still land on its own attempt row.
func TestFailureReport_MovesOnlyTheAttemptTheInvoiceWaitsOn(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Failure Report Rule")
	store := invoice.NewPostgresStore(db)
	outbox := &recordingOutbox{}
	store.SetOutboxEnqueuer(outbox)

	// Attempt rows are unique per (tenant, PaymentIntent), so every case
	// gets its own PaymentIntent ids: pid suffixes the case's tag.
	tag := ""
	pid := func(pi string) string { return pi + "_" + tag }

	type step func(t *testing.T, id string)
	none := func(*testing.T, string) {}
	webhook := func(pi string) step {
		return func(t *testing.T, id string) {
			if _, _, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, id, failureReport(pid(pi), "declined"), nil); err != nil {
				t.Fatalf("webhook report %s: %v", pi, err)
			}
		}
	}
	decline := func(pi string) step {
		return func(t *testing.T, id string) {
			stampOutcome(t, store, ctx, tenantID, id, domain.PaymentFailed, pid(pi), "declined")
		}
	}
	processing := func(pi string) step {
		return func(t *testing.T, id string) {
			stampOutcome(t, store, ctx, tenantID, id, domain.PaymentProcessing, pid(pi), "")
		}
	}
	// veloxAttempt writes the attempt row the charge call writes when Stripe
	// answers, before it reports (pi "" is a create whose id Velox never
	// learned).
	veloxAttempt := func(pi string) step {
		return func(t *testing.T, id string) {
			piID := ""
			if pi != "" {
				piID = pid(pi)
			}
			if err := store.RecordChargeAttempt(ctx, tenantID, domain.InvoiceChargeAttempt{
				InvoiceID: id, StripePaymentIntentID: piID, Trigger: domain.ChargeTriggerAutoCharge,
				Outcome: domain.ChargeAttemptPending, AmountCents: 5000, OccurredAt: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("record attempt %q: %v", pi, err)
			}
		}
	}
	stampFailedNoPI := func(t *testing.T, id string) {
		stampOutcome(t, store, ctx, tenantID, id, domain.PaymentFailed, "", "stripe not configured")
	}
	park := func(t *testing.T, id string) {
		stampOutcome(t, store, ctx, tenantID, id, domain.PaymentUnknown, "", "timeout")
	}
	adopt := func(pi string) step {
		return func(t *testing.T, id string) {
			if ok, err := store.AdoptPaymentIntentIfParked(ctx, tenantID, id, pid(pi)); err != nil || !ok {
				t.Fatalf("adopt %s: ok=%v err=%v", pi, ok, err)
			}
		}
	}
	markPaidWith := func(pi string) step {
		return func(t *testing.T, id string) {
			piID := ""
			if pi != "" {
				piID = pid(pi)
			}
			if _, err := store.MarkPaid(ctx, tenantID, id, piID, time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)); err != nil {
				t.Fatalf("mark paid: %v", err)
			}
		}
	}
	markPaid := func(t *testing.T, id string) {
		if _, err := store.MarkPaid(ctx, tenantID, id, pid("pi_OK"), time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("mark paid: %v", err)
		}
	}
	steps := func(ss ...step) step {
		return func(t *testing.T, id string) {
			for _, s := range ss {
				s(t, id)
			}
		}
	}

	const (
		ownDeclineReport = iota // the charge call, charged at the seq read before `during`
		webhookReport
		hostedReport
	)
	cases := []struct {
		name           string
		before, during step
		source         int
		pi             string
		recorded       bool
		notice         bool
	}{
		// The charge call's own decline.
		{"own decline, nothing else happened", none, none, ownDeclineReport, "pi_X", true, true},
		{"own decline after an earlier declined attempt", decline("pi_W"), none, ownDeclineReport, "pi_X", true, true},
		{"own decline after its webhook already recorded it", none, webhook("pi_X"), ownDeclineReport, "pi_X", true, false},
		{"own decline while the previous attempt's webhook is redelivered", decline("pi_W"), webhook("pi_W"), ownDeclineReport, "pi_X", true, true},
		{"own decline after another attempt recorded during the call", none, webhook("pi_Z"), ownDeclineReport, "pi_X", false, false},

		// Webhook or reconciler reports.
		{"webhook for the first attempt, before its charge call reports", none, none, webhookReport, "pi_X", true, true},
		{"webhook redelivery of the recorded decline", decline("pi_X"), none, webhookReport, "pi_X", true, false},
		{"stale webhook for an older attempt, invoice failed on a newer one", steps(decline("pi_A"), decline("pi_B")), none, webhookReport, "pi_A", false, false},
		{"stale webhook for an older attempt, newer one processing", steps(decline("pi_A"), processing("pi_B")), none, webhookReport, "pi_A", false, false},
		{"webhook for the processing attempt itself", processing("pi_B"), none, webhookReport, "pi_B", true, true},

		// A hosted Checkout attempt never moves the invoice.
		{"hosted decline on an invoice with no attempt", none, none, hostedReport, "pi_H", false, false},
		{"hosted decline while an attempt is processing", processing("pi_B"), none, hostedReport, "pi_H", false, false},
		{"hosted attempt a parked invoice adopted", steps(veloxAttempt(""), park, adopt("pi_H")), none, hostedReport, "pi_H", true, true},

		// The attempt history decides a report about a PaymentIntent the
		// invoice does not hold.
		{"lost charge report: webhook for Velox's newest attempt", steps(veloxAttempt("pi_A"), decline("pi_A"), veloxAttempt("pi_B")), none, webhookReport, "pi_B", true, true},
		{"stale decline superseded by a newer Velox attempt", steps(veloxAttempt("pi_A"), decline("pi_A"), veloxAttempt("pi_B"), decline("pi_B")), none, webhookReport, "pi_A", false, false},
		{"stale decline on a parked invoice", steps(veloxAttempt("pi_A"), decline("pi_A"), veloxAttempt(""), park), none, webhookReport, "pi_A", false, false},
		{"the parked attempt's own decline", steps(veloxAttempt("pi_A"), decline("pi_A"), veloxAttempt(""), park), none, webhookReport, "pi_P", true, true},
		{"stale decline after a failure with no PaymentIntent", steps(veloxAttempt("pi_A"), decline("pi_A"), veloxAttempt(""), stampFailedNoPI), none, webhookReport, "pi_A", false, false},

		// A paid invoice is never moved, by any source.
		{"webhook decline on a paid invoice", markPaid, none, webhookReport, "pi_X", false, false},
		{"late failure for the PaymentIntent that then succeeded (3DS retry on one PI)", markPaidWith("pi_X"), none, webhookReport, "pi_X", false, false},
		{"webhook decline on an invoice paid by credits (no PaymentIntent)", markPaidWith(""), none, webhookReport, "pi_X", false, false},
		{"stale decline while the invoice waits on an attempt whose row was lost", steps(veloxAttempt("pi_A"), decline("pi_A"), processing("pi_B")), none, webhookReport, "pi_A", false, false},
		{"own decline on an invoice paid during the call", none, markPaid, ownDeclineReport, "pi_X", false, false},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tag = string(rune('A' + i))
			pi := pid(tc.pi)
			inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-FR-"+tag)
			tc.before(t, inv.ID)
			attempt, err := store.Get(ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("attempt read: %v", err)
			}
			tc.during(t, inv.ID)
			before, err := store.Get(ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("get before report: %v", err)
			}
			events := len(outbox.events)

			report := domain.PaymentFailureReport{PaymentIntentID: pi, Message: "the decline"}
			switch tc.source {
			case ownDeclineReport:
				seq := attempt.ChargeAttemptSeq
				report.ChargedAtSeq = &seq
			case hostedReport:
				report.External = true
			}
			got, res, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, inv.ID, report, nil)
			if err != nil {
				t.Fatalf("report: %v", err)
			}
			if res.Recorded != tc.recorded || res.FirstNotice != tc.notice {
				t.Fatalf("result recorded=%v notice=%v, want %v/%v", res.Recorded, res.FirstNotice, tc.recorded, tc.notice)
			}
			after, err := store.Get(ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("get after: %v", err)
			}
			if got.PaymentStatus != after.PaymentStatus || got.StripePaymentIntentID != after.StripePaymentIntentID || got.ChargeAttemptSeq != after.ChargeAttemptSeq {
				t.Errorf("returned row %s/%q/seq %d differs from disk %s/%q/seq %d",
					got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq,
					after.PaymentStatus, after.StripePaymentIntentID, after.ChargeAttemptSeq)
			}

			wantEvents := events
			if tc.notice {
				wantEvents++
			}
			if len(outbox.events) != wantEvents {
				t.Errorf("payment.failed enqueues %d → %d, want %d", events, len(outbox.events), wantEvents)
			}

			switch {
			case !tc.recorded:
				if after.PaymentStatus != before.PaymentStatus || after.StripePaymentIntentID != before.StripePaymentIntentID ||
					after.ChargeAttemptSeq != before.ChargeAttemptSeq || after.LastPaymentError != before.LastPaymentError {
					t.Fatalf("an unrecorded report moved the invoice: %s/%q/seq %d → %s/%q/seq %d",
						before.PaymentStatus, before.StripePaymentIntentID, before.ChargeAttemptSeq,
						after.PaymentStatus, after.StripePaymentIntentID, after.ChargeAttemptSeq)
				}
			case before.PaymentStatus == domain.PaymentFailed && before.StripePaymentIntentID == pi:
				// Re-recording the decline the invoice already holds moves only
				// the notice marker: the seq is a charge in flight's CAS.
				if after.ChargeAttemptSeq != before.ChargeAttemptSeq {
					t.Errorf("seq %d → %d re-recording a held decline, want unchanged", before.ChargeAttemptSeq, after.ChargeAttemptSeq)
				}
			default:
				if after.PaymentStatus != domain.PaymentFailed || after.StripePaymentIntentID != pi {
					t.Fatalf("row %s/%q, want failed/%q", after.PaymentStatus, after.StripePaymentIntentID, pi)
				}
				if after.ChargeAttemptSeq != before.ChargeAttemptSeq+1 {
					t.Errorf("seq %d → %d, want +1 for a new outcome", before.ChargeAttemptSeq, after.ChargeAttemptSeq)
				}
			}

			// Every report lands on its own attempt row — as failed, unless the
			// attempt already succeeded, which is terminal (a 3DS PaymentIntent
			// can fail then succeed on the customer's second try).
			attempts, err := store.ListChargeAttemptsByInvoice(ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("list attempts: %v", err)
			}
			found := false
			for _, a := range attempts {
				if a.StripePaymentIntentID == pi {
					found = a.Outcome == domain.ChargeAttemptFailed || a.Outcome == domain.ChargeAttemptSucceeded
				}
			}
			if !found {
				t.Errorf("no failed attempt row for %s: %+v", pi, attempts)
			}
		})
	}

	t.Run("a report needs a PaymentIntent", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-FR-NOPI")
		if _, _, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, inv.ID, failureReport("", "declined"), nil); err == nil {
			t.Fatal("reported a failure with no PaymentIntent; want a refusal — it is stamped, not reported")
		}
	})
}

// TestRecordChargeAttempt_ClaimsARowAReportInsertedFirst: when a decline's
// webhook beats the charge call, the report inserts the attempt row as
// 'external'. The charge call made the attempt, so its write must claim the
// row — the timeline label and the attempt order (which decides whether a
// later report moves the invoice) both read the trigger.
func TestRecordChargeAttempt_ClaimsARowAReportInsertedFirst(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Attempt Trigger Claim")
	store := invoice.NewPostgresStore(db)
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-TRIG-1")

	if _, _, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, inv.ID, failureReport("pi_first", "declined"), nil); err != nil {
		t.Fatalf("webhook report: %v", err)
	}
	if err := store.RecordChargeAttempt(ctx, tenantID, domain.InvoiceChargeAttempt{
		InvoiceID: inv.ID, StripePaymentIntentID: "pi_first", Trigger: domain.ChargeTriggerDunningRetry,
		Outcome: domain.ChargeAttemptFailed, AmountCents: 5000, OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	attempts, err := store.ListChargeAttemptsByInvoice(ctx, tenantID, inv.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts %+v err %v, want exactly one row", attempts, err)
	}
	if attempts[0].Trigger != domain.ChargeTriggerDunningRetry {
		t.Fatalf("trigger %q, want dunning_retry — the charge call's write must claim a row the report inserted first", attempts[0].Trigger)
	}

	// A hosted attempt's row stays 'external': nothing claims it.
	if _, _, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, inv.ID,
		domain.PaymentFailureReport{PaymentIntentID: "pi_hosted", Message: "declined", External: true}, nil); err != nil {
		t.Fatalf("hosted report: %v", err)
	}
	attempts, _ = store.ListChargeAttemptsByInvoice(ctx, tenantID, inv.ID)
	for _, a := range attempts {
		if a.StripePaymentIntentID == "pi_hosted" && a.Trigger != domain.ChargeTriggerExternal {
			t.Fatalf("hosted attempt trigger %q, want external", a.Trigger)
		}
	}
}
