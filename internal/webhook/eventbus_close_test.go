package webhook

import "testing"

// TestEventBus_CloseAll pins the shutdown wiring (2026-08-30): every
// subscriber channel closes (the SSE loop exits, http.Server.Shutdown's
// idle wait completes in ms instead of 30s), a Subscribe that races
// shutdown gets an already-closed channel, and CloseAll is idempotent.
// Mutation-verify: make CloseAll skip close(sub.ch) → the receive blocks
// and the test times out.
func TestEventBus_CloseAll(t *testing.T) {
	b := NewEventBus()
	a, cancelA := b.Subscribe("t1")
	c, cancelC := b.Subscribe("t2")
	defer cancelA()
	defer cancelC()

	b.CloseAll()

	for name, ch := range map[string]<-chan StreamFrame{"t1": a, "t2": c} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatalf("%s: got a frame after CloseAll, want closed channel", name)
			}
		default:
			t.Fatalf("%s: channel still open after CloseAll — the SSE loop would hold the drain", name)
		}
	}

	late, cancelLate := b.Subscribe("t1")
	defer cancelLate()
	if _, ok := <-late; ok {
		t.Fatal("Subscribe after CloseAll must return a closed channel")
	}

	b.Publish("t1", StreamFrame{}) // must not panic on a closed bus
	b.CloseAll()                   // idempotent
	cancelA()                      // cancel after close must not double-close
}
