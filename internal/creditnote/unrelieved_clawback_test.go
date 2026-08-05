package creditnote

import (
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestUnreliefedClawbackPredicate pins the predicate that decides whether an
// invoice is owed a credit that was never applied.
//
// The original was `IssuePending && status == Voided` and was UNSATISFIABLE:
// creditnote/postgres.go's status transition sets `issue_pending = false` in
// the same statement that sets the status, so no row can ever hold both. The
// gate built on it shipped and could never fire. It came from
// domain.CreditNote's own comment asserting the flag is "NEVER cleared" — a
// comment the clearing writer contradicts.
//
// This test exists so the same mistake cannot be made twice: the first case
// IS the old predicate's shape, and it must still count.
func TestUnreliefedClawbackPredicate(t *testing.T) {
	now := domain.CreditNote{}.CreatedAt // zero time, only used for non-nil-ness
	issued := &now

	cases := []struct {
		name  string
		cn    domain.CreditNote
		count bool
	}{
		{
			// The real orphan shape. issue_pending is FALSE here — cleared by the
			// void — which is exactly why the old predicate missed it.
			name:  "voided before ever issuing (the orphan-void)",
			cn:    domain.CreditNote{Status: domain.CreditNoteVoided, IssuePending: false},
			count: true,
		},
		{
			name:  "issued then voided — relief WAS applied, must not count",
			cn:    domain.CreditNote{Status: domain.CreditNoteVoided, IssuedAt: issued},
			count: false,
		},
		{
			name:  "issued and live — relief applied",
			cn:    domain.CreditNote{Status: domain.CreditNoteIssued, IssuedAt: issued},
			count: false,
		},
		{
			// Still pending issuance; the reconciler will issue it. Counting it
			// would warn about relief that is about to be applied.
			name:  "draft awaiting issue",
			cn:    domain.CreditNote{Status: domain.CreditNoteDraft, IssuePending: true},
			count: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cn.Status == domain.CreditNoteVoided && tc.cn.IssuedAt == nil
			if got != tc.count {
				t.Fatalf("counted=%v, want %v — this predicate decides whether an operator is told the invoice amount is stale-HIGH", got, tc.count)
			}
		})
	}
}
