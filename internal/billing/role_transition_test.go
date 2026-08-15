package billing

import (
	"testing"
	"time"
)

// The behaviour under test is "says something exactly when the state changes,
// and stays silent otherwise". Both halves of that matter and they fail in
// opposite directions: silence on a change is the bug this replaces (the skip
// logged at Debug, so production saw nothing), and noise on no-change is the
// bug a naive fix introduces (one line per replica per tick, forever, which
// operators then filter out — leaving them exactly as blind as before).

func TestRoleState_LogsOnceOnEachTransition(t *testing.T) {
	var r roleState
	base := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	// First observation always speaks: an operator needs to know which role a
	// freshly started replica took, and "no news" is indistinguishable from a
	// replica that never ran.
	if msg, _ := r.observe(base, false); msg == "" {
		t.Fatal("first observation must log; a silent start is indistinguishable from a dead replica")
	}

	// Following, tick after tick after tick. This is the steady state of every
	// healthy multi-replica deployment and must produce nothing.
	for i := 1; i <= 500; i++ {
		if msg, _ := r.observe(base.Add(time.Duration(i)*time.Minute), false); msg != "" {
			t.Fatalf("tick %d logged %q while still following; steady state must be silent", i, msg)
		}
	}

	// Leader dies, this replica takes over. One line, carrying how long it had
	// been following — which is the operator's answer to "how long was the
	// handover".
	msg, prior := r.observe(base.Add(501*time.Minute), true)
	if msg == "" {
		t.Fatal("taking leadership must log")
	}
	if prior != 501*time.Minute {
		t.Fatalf("expected the previous state's duration (501m), got %v", prior)
	}

	// Leading steadily: silent again.
	for i := 502; i <= 600; i++ {
		if m, _ := r.observe(base.Add(time.Duration(i)*time.Minute), true); m != "" {
			t.Fatalf("tick %d logged %q while still leading; steady state must be silent", i, m)
		}
	}

	// And handing leadership back logs once more.
	if m, p := r.observe(base.Add(601*time.Minute), false); m == "" {
		t.Fatal("losing leadership must log")
	} else if p != 100*time.Minute {
		t.Fatalf("expected 100m of leadership, got %v", p)
	}
}

// TestRoleState_FlappingLogsEveryChange guards the opposite failure: a
// de-bounce that swallowed rapid alternation would hide genuine lock
// thrash, which is a real symptom worth seeing.
func TestRoleState_FlappingLogsEveryChange(t *testing.T) {
	var r roleState
	base := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	r.observe(base, false)

	logged := 0
	for i := 1; i <= 20; i++ {
		if msg, _ := r.observe(base.Add(time.Duration(i)*time.Second), i%2 == 1); msg != "" {
			logged++
		}
	}
	if logged != 20 {
		t.Fatalf("expected 20 lines from 20 alternating observations, got %d — lock thrash "+
			"would be invisible", logged)
	}
}
