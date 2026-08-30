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

func TestFence_TokenRoundTripAndMissingTokenFailsLoud(t *testing.T) {
	if _, _, err := Fence(context.Background()); !errors.Is(err, ErrNoToken) {
		t.Fatalf("bare ctx: want ErrNoToken, got %v", err)
	}
	ctx := WithToken(context.Background(), RoleBilling, 42)
	role, token, err := Fence(ctx)
	if err != nil || role != RoleBilling || token != 42 {
		t.Fatalf("Fence = (%s, %d, %v)", role, token, err)
	}
	if _, _, err := Fence(WithToken(context.Background(), RoleBilling, 0)); !errors.Is(err, ErrNoToken) {
		t.Fatalf("token 0 must not pass the fence, got %v", err)
	}
}

func TestDoubles(t *testing.T) {
	ran := false
	led, err := AlwaysLead{}.Lead(context.Background(), RoleDunning, time.Minute, func(ctx context.Context) {
		if _, tok, err := Fence(ctx); err != nil || tok != 1 {
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
