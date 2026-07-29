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
		if !recorded.Equal(dispatched) {
			t.Errorf("recorded = %s, want the real dispatch instant %s", recorded, dispatched)
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
		if !recorded.IsZero() || isSim {
			t.Errorf("unanchored row must carry no Recorded subline and stay unflagged (recorded=%s sim=%v) — the calendars coincide", recorded, isSim)
		}
	})
}

// TestLifecycleRecordedStamps pins the audit→lifecycle join (ADR-104
// Invariant A, corrected boundary №2 — operator nudge, same day as the
// credit-note one). Exact keys, frozen-vocabulary discriminators for the
// update-flavored transitions, earliest-row-wins on duplicates, unknown
// actions ignored.
func TestLifecycleRecordedStamps(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 7, 29, h, 0, 0, 0, time.UTC) }
	// Query order is newest-first — build the list that way.
	entries := []domain.AuditEntry{
		{Action: "update", Metadata: map[string]any{"action": "payment_recorded"}, CreatedAt: at(16)},
		{Action: "update", Metadata: map[string]any{"action": "marked_uncollectible"}, CreatedAt: at(15)},
		{Action: "send", CreatedAt: at(14)}, // not a transition — ignored
		{Action: "update", Metadata: map[string]any{"action": "line_item_added"}, CreatedAt: at(13)}, // other update flavor — ignored
		{Action: "void", CreatedAt: at(12)},
		{Action: "finalize", CreatedAt: at(11)},
		{Action: "finalize", CreatedAt: at(10)}, // duplicate: EARLIEST (10:00) must win
		{Action: "create", CreatedAt: at(9)},
	}
	got := lifecycleRecordedStamps(entries)
	want := map[string]time.Time{
		"invoice.created":              at(9),
		"invoice.finalized":            at(10),
		"invoice.voided":               at(12),
		"invoice.marked_uncollectible": at(15),
		"invoice.paid":                 at(16),
	}
	if len(got) != len(want) {
		t.Fatalf("stamp count = %d, want %d (got %v)", len(got), len(want), got)
	}
	for k, w := range want {
		if !got[k].Equal(w) {
			t.Errorf("%s = %s, want %s", k, got[k], w)
		}
	}
}

// TestSortInvoiceTimeline_RealityPass pins the ADR-104 ordering
// amendment: within one story-instant group, FULLY-stamped rows order by
// their real-world sequence — a frozen-clock invoice reads like a
// wall-clock one — while any bare row gates the whole group back to the
// causal ladder (a partially-stamped group is legacy data, and placing
// bare rows against stamped ones would fabricate a sequence).
func TestSortInvoiceTimeline_RealityPass(t *testing.T) {
	instant := time.Date(2027, 6, 1, 12, 0, 0, 0, time.UTC)
	wall := func(h, m int) time.Time { return time.Date(2026, 7, 29, h, m, 0, 0, time.UTC) }

	t.Run("all-stamped group orders by real sequence, across ranks", func(t *testing.T) {
		// Real sequence deliberately CONTRADICTS the ladder: the email
		// went out in the morning, the CN and the void came later.
		events := []timelineEvent{
			{EventType: "invoice.voided", sortAt: instant, tieRank: rankLifecycleTerminal, recordedSort: wall(15, 0)},
			{EventType: "credit_note.issued", sortAt: instant, tieRank: rankCreditNote, recordedSort: wall(14, 0)},
			{EventType: "email.invoice", sortAt: instant, tieRank: rankEmail, recordedSort: wall(10, 0)},
			{EventType: "invoice.finalized", sortAt: instant, tieRank: rankInvoiceFinalized, recordedSort: wall(9, 0)},
			{EventType: "invoice.created", sortAt: instant, tieRank: rankInvoiceCreated, recordedSort: wall(8, 59)},
		}
		sortInvoiceTimeline(events)
		want := []string{"invoice.created", "invoice.finalized", "email.invoice", "credit_note.issued", "invoice.voided"}
		for i, w := range want {
			if events[i].EventType != w {
				t.Fatalf("position %d: got %s, want %s (real sequence must win: %v)", i, events[i].EventType, w, typesOf(events))
			}
		}
	})

	t.Run("one bare row gates the whole group back to the ladder", func(t *testing.T) {
		// Same shape, but the CN is a pre-0164 row with no stamp: the
		// group must render in ladder order — today's behavior, never a
		// fabricated sequence.
		events := []timelineEvent{
			{EventType: "email.invoice", sortAt: instant, tieRank: rankEmail, recordedSort: wall(10, 0)},
			{EventType: "credit_note.issued", sortAt: instant, tieRank: rankCreditNote}, // bare
			{EventType: "invoice.finalized", sortAt: instant, tieRank: rankInvoiceFinalized, recordedSort: wall(9, 0)},
		}
		sortInvoiceTimeline(events)
		want := []string{"invoice.finalized", "credit_note.issued", "email.invoice"}
		for i, w := range want {
			if events[i].EventType != w {
				t.Fatalf("position %d: got %s, want %s (bare row must gate to ladder: %v)", i, events[i].EventType, w, typesOf(events))
			}
		}
	})

	t.Run("equal recorded stamps (same tx) fall back to the ladder", func(t *testing.T) {
		txStamp := wall(9, 0)
		events := []timelineEvent{
			{EventType: "email.payment_setup_request", sortAt: instant, tieRank: rankEmail, recordedSort: txStamp},
			{EventType: "dunning.started", sortAt: instant, tieRank: rankDunningStarted, recordedSort: txStamp},
			{EventType: "invoice.finalized", sortAt: instant, tieRank: rankInvoiceFinalized, recordedSort: txStamp},
		}
		sortInvoiceTimeline(events)
		want := []string{"invoice.finalized", "dunning.started", "email.payment_setup_request"}
		for i, w := range want {
			if events[i].EventType != w {
				t.Fatalf("position %d: got %s, want %s (tx-stable stamps must fall to ladder: %v)", i, events[i].EventType, w, typesOf(events))
			}
		}
	})

	t.Run("cross-instant ordering untouched — story time stays primary", func(t *testing.T) {
		// A late-recorded row at an EARLIER story instant must stay above
		// an early-recorded row at a LATER story instant (the anchored
		// webhook-settle case that kills wall-primary designs).
		events := []timelineEvent{
			{EventType: "later.story", sortAt: instant.Add(time.Hour), tieRank: rankInvoiceCreated, recordedSort: wall(9, 0)},
			{EventType: "earlier.story", sortAt: instant, tieRank: rankLifecycleTerminal, recordedSort: wall(18, 0)},
		}
		sortInvoiceTimeline(events)
		if events[0].EventType != "earlier.story" {
			t.Fatalf("story time must dominate recorded time: %v", typesOf(events))
		}
	})
}
