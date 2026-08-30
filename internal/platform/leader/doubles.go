package leader

import (
	"context"
	"time"
)

// AlwaysLead is a test Gate that leads every call with token 1.
type AlwaysLead struct{}

// Lead implements Gate.
func (AlwaysLead) Lead(ctx context.Context, role Role, _ time.Duration, work func(context.Context)) (bool, error) {
	work(WithToken(ctx, role, 1))
	return true, nil
}

// NeverLead is a test Gate that never leads (a follower).
type NeverLead struct{}

// Lead implements Gate.
func (NeverLead) Lead(context.Context, Role, time.Duration, func(context.Context)) (bool, error) {
	return false, nil
}

// ErrGate is a test Gate whose acquire fails.
type ErrGate struct{ Err error }

// Lead implements Gate.
func (g ErrGate) Lead(context.Context, Role, time.Duration, func(context.Context)) (bool, error) {
	return false, g.Err
}
