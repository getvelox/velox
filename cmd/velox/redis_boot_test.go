package main

import (
	"errors"
	"testing"
)

// The Redis boot verdict: an open/ping error is fatal in production only.
// config.validateFatal already refuses an UNSET REDIS_URL in production;
// this covers invalid/unreachable. Mutation-verify: flip the env check →
// the (production, err) row fails.
func TestRedisBootFatal(t *testing.T) {
	err := errors.New("dial tcp: connection refused")
	cases := []struct {
		env  string
		err  error
		want bool
	}{
		{"production", err, true},
		{"production", nil, false},
		{"staging", err, false},
		{"local", err, false},
		{"local", nil, false},
	}
	for _, c := range cases {
		if got := redisBootFatal(c.env, c.err); got != c.want {
			t.Errorf("redisBootFatal(%q, %v) = %v, want %v", c.env, c.err, got, c.want)
		}
	}
}
