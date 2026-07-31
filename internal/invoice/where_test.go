package invoice

import (
	"strings"
	"testing"
	"time"
)

// buildInvWhere is the SQL gate for every operator-facing invoice list
// filter. These are pure unit tests (no DB) on the generated WHERE
// string + positional args — same style as order_by_test.go.

func TestBuildInvWhere_Search(t *testing.T) {
	where, args := buildInvWhere(ListFilter{Search: "INV-2026"})
	if !strings.Contains(where, "invoice_number ILIKE $1") {
		t.Fatalf("want invoice_number ILIKE clause, got %q", where)
	}
	if len(args) != 1 || args[0] != "%INV-2026%" {
		t.Fatalf("want single pattern arg %%INV-2026%%, got %v", args)
	}
}

func TestBuildInvWhere_SearchEscapesLikeMeta(t *testing.T) {
	// "100%" must match literally, not as a wildcard that matches
	// every invoice.
	_, args := buildInvWhere(ListFilter{Search: `100%_\`})
	want := `%100\%\_\\%`
	if len(args) != 1 || args[0] != want {
		t.Fatalf("want escaped pattern %q, got %v", want, args)
	}
}

func TestBuildInvWhere_CreatedRange(t *testing.T) {
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 31, 23, 59, 59, 0, time.UTC)
	where, args := buildInvWhere(ListFilter{CreatedFrom: from, CreatedTo: to})
	if !strings.Contains(where, "created_at >= $1") || !strings.Contains(where, "created_at <= $2") {
		t.Fatalf("want inclusive created_at range clauses, got %q", where)
	}
	if len(args) != 2 || args[0] != from || args[1] != to {
		t.Fatalf("want [from to] args, got %v", args)
	}
}

func TestBuildInvWhere_Overdue(t *testing.T) {
	where, args := buildInvWhere(ListFilter{Overdue: true})
	for _, frag := range []string{"status = $1", "due_at IS NOT NULL", "due_at < now()", "payment_status NOT IN ($2, $3, $4)"} {
		if !strings.Contains(where, frag) {
			t.Fatalf("overdue clause missing %q in %q", frag, where)
		}
	}
	// 'unknown' joined 'processing' as an excluded status (ADR-107): the
	// clause's own comment has always said "not mid-payment", and IsInFlight
	// means processing OR unknown. Listing a PARKED invoice as past-due and
	// chaseable pointed an operator at the one invoice no charge path will
	// touch, and whose money may already have left the customer's account.
	if len(args) != 4 || args[0] != "finalized" || args[1] != "succeeded" ||
		args[2] != "processing" || args[3] != "unknown" {
		t.Fatalf("want [finalized succeeded processing unknown] args, got %v", args)
	}
}

// Composition: every filter set at once must produce sequentially
// numbered placeholders matching the args slice ordering — an
// off-by-one here silently binds a filter to the wrong column value.
func TestBuildInvWhere_ComposedPlaceholderNumbering(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	where, args := buildInvWhere(ListFilter{
		CustomerID:    "cus_1",
		Status:        "finalized",
		PaymentStatus: "failed",
		Search:        "INV",
		CreatedFrom:   from,
		Overdue:       true,
		IDs:           []string{"inv_a", "inv_b"},
	})
	// 1 customer + 1 status + 1 payment_status + 1 search + 1 from +
	// 4 overdue + 2 ids = 11 args.
	if len(args) != 11 {
		t.Fatalf("want 11 args, got %d: %v", len(args), args)
	}
	// The highest placeholder must equal the arg count.
	if !strings.Contains(where, "$11") || strings.Contains(where, "$12") {
		t.Fatalf("placeholder numbering does not end at $11: %q", where)
	}
	if !strings.Contains(where, "customer_id = $1") {
		t.Fatalf("first clause should bind customer_id to $1: %q", where)
	}
}
