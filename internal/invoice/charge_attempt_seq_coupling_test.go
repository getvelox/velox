package invoice

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestChargeAttemptSeqBumpedByEveryPIStamp mechanises the invariant that makes
// the Stripe charge idempotency key correct: charge_attempt_seq must move
// exactly when a charge attempt outcome is recorded.
//
// "An outcome was recorded" has one syntactic tell in this store — the UPDATE
// stamps stripe_payment_intent_id. So the rule is checkable by construction:
// every UPDATE on invoices that sets stripe_payment_intent_id must bump
// charge_attempt_seq in the SAME statement.
//
// Why a source-scanning test and not a convention: this is a class-C2 hazard
// (playbook §1) — the invariant is spread across THREE writers today
// (UpdatePayment, MarkPaymentFailedReportingTransition,
// markPaidReportingTransition), and a fourth added later would fail silently in
// the worst direction. Missing the bump means a RECORDED decline reuses its
// key, Stripe hands back the declined PaymentIntent forever, and the invoice
// can never be paid — a liveness sink (class G) with no error anywhere.
//
// The reverse direction is checked too: a bump without a PI stamp means some
// unrelated write is re-seeding the key, which is the exact drift this column
// was introduced to end (#677 → #678).
func TestChargeAttemptSeqBumpedByEveryPIStamp(t *testing.T) {
	const (
		piStamp  = "stripe_payment_intent_id ="
		seqBump  = "charge_attempt_seq = charge_attempt_seq + 1"
		fileName = "postgres.go"
	)

	src, err := os.ReadFile(fileName)
	if err != nil {
		t.Fatalf("read %s: %v", fileName, err)
	}

	// Each UPDATE ... on the invoices table, up to its terminating clause.
	updateStmt := regexp.MustCompile(`(?s)UPDATE invoices.*?(?:WHERE|RETURNING)`)
	stmts := updateStmt.FindAllString(string(src), -1)
	if len(stmts) == 0 {
		t.Fatal("found no UPDATE invoices statements — the scanner is broken, not the code")
	}

	var stamps, bumps int
	for _, stmt := range stmts {
		// A predicate reference (WHERE stripe_payment_intent_id = $1) is a read,
		// not a stamp; the regex stops before WHERE, so anything left is a SET.
		hasStamp := strings.Contains(stmt, piStamp)
		hasBump := strings.Contains(stmt, seqBump)
		switch {
		case hasStamp && !hasBump:
			t.Errorf("an UPDATE stamps stripe_payment_intent_id without bumping charge_attempt_seq — "+
				"a recorded decline would reuse its idempotency key and Stripe would return the DECLINED "+
				"PaymentIntent forever, so the invoice could never be paid:\n%s", stmt)
		case hasBump && !hasStamp:
			t.Errorf("an UPDATE bumps charge_attempt_seq without stamping stripe_payment_intent_id — "+
				"only a recorded attempt outcome may re-seed the charge idempotency key; re-seeding it "+
				"from an unrelated write is the drift #678 removed:\n%s", stmt)
		}
		if hasStamp {
			stamps++
		}
		if hasBump {
			bumps++
		}
	}

	// Guard against the scanner silently matching nothing after a refactor:
	// the three known outcome writers must all be seen.
	if stamps < 3 {
		t.Errorf("expected at least 3 PI-stamping UPDATEs (UpdatePayment, MarkPaymentFailedReportingTransition, "+
			"markPaidReportingTransition), found %d — did the scanner stop matching?", stamps)
	}
	if bumps != stamps {
		t.Errorf("PI stamps (%d) and seq bumps (%d) disagree", stamps, bumps)
	}
}
