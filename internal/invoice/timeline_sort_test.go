package invoice

import (
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestSortInvoiceTimeline_CausalTies locks the 2026-07-19 audit findings:
// a frozen-clock close cascade stamps finalize → dunning start → retries →
// escalation → write-off → resolve at ONE instant, and the old second-
// truncated string sort fell back to source-major insertion — rendering
// "Marked uncollectible" above the escalation that caused it, and
// "Invoice paid" above the same-second retry that collected it. Ties now
// order by causal rank; distinct sub-second instants order by full
// precision (the serialized string can't carry them).
func TestSortInvoiceTimeline_CausalTies(t *testing.T) {
	instant := time.Date(2027, 3, 8, 18, 30, 0, 0, time.UTC)

	t.Run("same-instant cascade orders causally regardless of insertion", func(t *testing.T) {
		// Deliberately inserted in the OLD source-major order that
		// rendered anti-causally: lifecycle first, dunning last.
		events := []timelineEvent{
			{EventType: "invoice.uncollectible", sortAt: instant, tieRank: rankLifecycleTerminal},
			{EventType: "invoice.finalized", sortAt: instant, tieRank: rankInvoiceFinalized},
			{EventType: "invoice.created", sortAt: instant, tieRank: rankInvoiceCreated},
			{EventType: "dunning.retry_attempted", sortAt: instant, tieRank: rankRetryAttempted},
			{EventType: "dunning.escalated", sortAt: instant, tieRank: rankEscalated},
			{EventType: "dunning.started", sortAt: instant, tieRank: rankDunningStarted},
		}
		sortInvoiceTimeline(events)
		want := []string{
			"invoice.created", "invoice.finalized", "dunning.started",
			"dunning.retry_attempted", "dunning.escalated", "invoice.uncollectible",
		}
		for i, w := range want {
			if events[i].EventType != w {
				t.Fatalf("position %d: got %s, want %s (full order: %v)", i, events[i].EventType, w, typesOf(events))
			}
		}
	})

	t.Run("failed retry precedes same-instant settle", func(t *testing.T) {
		events := []timelineEvent{
			{EventType: "invoice.paid", sortAt: instant, tieRank: rankLifecycleTerminal},
			{EventType: "dunning.retry_attempted", sortAt: instant, tieRank: rankRetryAttempted},
		}
		sortInvoiceTimeline(events)
		if events[0].EventType != "dunning.retry_attempted" {
			t.Errorf("the retry that collected the invoice must render before 'Invoice paid': got %v", typesOf(events))
		}
	})

	t.Run("sub-second precision beats rank", func(t *testing.T) {
		// Two CNs 36ms apart — the serialized string collides at second
		// granularity; full-precision sortAt must order them, with the
		// later-written CN below the earlier one whatever the rank says.
		events := []timelineEvent{
			{EventType: "cn.second", sortAt: instant.Add(36 * time.Millisecond), tieRank: rankCreditNote},
			{EventType: "cn.first", sortAt: instant.Add(4 * time.Millisecond), tieRank: rankCreditNote},
			{EventType: "late.lifecycle", sortAt: instant.Add(900 * time.Millisecond), tieRank: rankInvoiceCreated},
		}
		sortInvoiceTimeline(events)
		if events[0].EventType != "cn.first" || events[1].EventType != "cn.second" || events[2].EventType != "late.lifecycle" {
			t.Errorf("full-precision instants must dominate rank: got %v", typesOf(events))
		}
	})

	t.Run("dunningEventRank maps every kind causally", func(t *testing.T) {
		causal := dunningEventRank(domain.DunningEventStarted) < dunningEventRank(domain.DunningEventRetryAttempted) &&
			dunningEventRank(domain.DunningEventRetryAttempted) < dunningEventRank(domain.DunningEventEscalated) &&
			dunningEventRank(domain.DunningEventEscalated) < rankLifecycleTerminal &&
			rankLifecycleTerminal < dunningEventRank(domain.DunningEventResolved)
		if !causal {
			t.Error("dunning rank ordering broken: started < retry < escalated < terminal-lifecycle < resolved must hold")
		}
	})
}

func typesOf(events []timelineEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.EventType
	}
	return out
}

// TestSortInvoiceTimeline_FullLifeAtOneInstant is ADR-104's golden test:
// an invoice whose ENTIRE life happens at one frozen instant — the
// degenerate case a single clock advance produces — must render as the
// declared causal grammar, ending with the notification. Emails could
// never exercise their reserved rank before ADR-104 (they lived in a
// second card at wall time); this pins the rank the moment it goes live.
func TestSortInvoiceTimeline_FullLifeAtOneInstant(t *testing.T) {
	instant := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	// Inserted deliberately shuffled.
	events := []timelineEvent{
		{EventType: "email.payment_receipt", sortAt: instant, tieRank: rankEmail},
		{EventType: "credit_note.issued", sortAt: instant, tieRank: rankCreditNote},
		{EventType: "invoice.paid", sortAt: instant, tieRank: rankLifecycleTerminal},
		{EventType: "invoice.created", sortAt: instant, tieRank: rankInvoiceCreated},
		{EventType: "charge_attempt.succeeded", sortAt: instant, tieRank: rankChargeAttempt},
		{EventType: "invoice.finalized", sortAt: instant, tieRank: rankInvoiceFinalized},
	}
	sortInvoiceTimeline(events)
	want := []string{
		"invoice.created", "invoice.finalized", "charge_attempt.succeeded",
		"invoice.paid", "credit_note.issued", "email.payment_receipt",
	}
	for i, w := range want {
		if events[i].EventType != w {
			t.Fatalf("position %d: got %s, want %s (full order: %v)", i, events[i].EventType, w, typesOf(events))
		}
	}
}

// TestEmailRowInstant pins the ADR-104 anchoring decision per row shape.
func TestEmailRowInstant(t *testing.T) {
	wall := time.Date(2026, 7, 29, 5, 40, 0, 0, time.UTC)
	dispatched := wall.Add(3 * time.Second)
	sim := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	t.Run("anchored email positions at the billing instant, keeps wall as Recorded", func(t *testing.T) {
		ts, recorded, isSim := emailRowInstant(EmailEventRow{
			CreatedAt: wall, DispatchedAt: &dispatched, SimEffectiveAt: &sim,
		})
		if !ts.Equal(sim) {
			t.Errorf("primary instant = %s, want the billing anchor %s", ts, sim)
		}
		if recorded != dispatched.Format(time.RFC3339) {
			t.Errorf("recorded = %q, want the real dispatch instant %q", recorded, dispatched.Format(time.RFC3339))
		}
		if !isSim {
			t.Error("anchored row must be flagged simulated")
		}
	})

	t.Run("unanchored (live-mode / legacy) email keeps its wall stamp with no subline", func(t *testing.T) {
		ts, recorded, isSim := emailRowInstant(EmailEventRow{CreatedAt: wall})
		if !ts.Equal(wall) {
			t.Errorf("primary instant = %s, want wall %s", ts, wall)
		}
		if recorded != "" || isSim {
			t.Errorf("unanchored row must carry no Recorded subline and stay unflagged (recorded=%q sim=%v) — the calendars coincide", recorded, isSim)
		}
	})
}
