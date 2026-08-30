package leader

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestLease_ConstantsLeaveNoElectionGap pins the invariant every unfenced
// writer rests on: a holder that abandons itself (AbandonAfter after its
// last acknowledged renew, then at most one StatementTimeout for a statement
// already issued) stops before any successor can be elected (LeaseTTL after
// that same renew).
func TestLease_ConstantsLeaveNoElectionGap(t *testing.T) {
	if AbandonAfter+StatementTimeout >= LeaseTTL {
		t.Fatalf("AbandonAfter (%v) + StatementTimeout (%v) must be < LeaseTTL (%v)", AbandonAfter, StatementTimeout, LeaseTTL)
	}
	if HeartbeatEvery*2 > AbandonAfter {
		t.Fatalf("AbandonAfter (%v) must tolerate two missed beats at HeartbeatEvery (%v)", AbandonAfter, HeartbeatEvery)
	}
	if MaxPoll > LeaseTTL {
		t.Fatalf("MaxPoll (%v) must not exceed LeaseTTL (%v)", MaxPoll, LeaseTTL)
	}
}

// TestFence_PinnedToRole: a funnel proves the token for ITS role. A bare
// ctx fails loud (ErrNoToken); a ctx led for another role fails loud too
// (ErrWrongRole) — a billing tick reaching a dunning funnel is a wiring
// bug and must not pass on the strength of the wrong lease; token 0 never
// passes. Tests may attach several roles to one ctx (leadertest).
func TestFence_PinnedToRole(t *testing.T) {
	if _, err := Fence(context.Background(), RoleBilling); !errors.Is(err, ErrNoToken) {
		t.Fatalf("bare ctx: want ErrNoToken, got %v", err)
	}
	ctx := WithToken(context.Background(), RoleBilling, 42)
	if token, err := Fence(ctx, RoleBilling); err != nil || token != 42 {
		t.Fatalf("Fence(billing) = (%d, %v)", token, err)
	}
	if _, err := Fence(ctx, RoleDunning); !errors.Is(err, ErrWrongRole) {
		t.Fatalf("billing token at a dunning funnel: want ErrWrongRole, got %v", err)
	}
	if _, err := Fence(WithToken(context.Background(), RoleBilling, 0), RoleBilling); err == nil {
		t.Fatal("token 0 must not pass the fence")
	}
	both := WithToken(ctx, RoleDunning, 7)
	if token, err := Fence(both, RoleDunning); err != nil || token != 7 {
		t.Fatalf("second role: (%d, %v)", token, err)
	}
	if token, err := Fence(both, RoleBilling); err != nil || token != 42 {
		t.Fatalf("first role must survive adding a second: (%d, %v)", token, err)
	}
	if _, err := Fence(ctx, RoleDunning); !errors.Is(err, ErrWrongRole) {
		t.Fatal("WithToken must not mutate the parent ctx's token set")
	}
}

func TestDoubles(t *testing.T) {
	ran := false
	led, err := AlwaysLead{}.Lead(context.Background(), RoleDunning, time.Minute, func(ctx context.Context) {
		if tok, err := Fence(ctx, RoleDunning); err != nil || tok != 1 {
			t.Errorf("AlwaysLead must hand work a fenced ctx, got %v", err)
		}
		ran = true
	})
	if !led || err != nil || !ran {
		t.Fatalf("AlwaysLead: led=%v err=%v ran=%v", led, err, ran)
	}
	led, err = NeverLead{}.Lead(context.Background(), RoleDunning, time.Minute, func(context.Context) { t.Error("NeverLead ran work") })
	if led || err != nil {
		t.Fatalf("NeverLead: led=%v err=%v", led, err)
	}
	want := errors.New("boom")
	if _, err := (ErrGate{Err: want}).Lead(context.Background(), RoleDunning, time.Minute, nil); !errors.Is(err, want) {
		t.Fatalf("ErrGate: %v", err)
	}
}

func TestHolderID_Shape(t *testing.T) {
	a, b := holderID(), holderID()
	if a == b {
		t.Fatalf("holder ids must differ per mint: %s", a)
	}
}
