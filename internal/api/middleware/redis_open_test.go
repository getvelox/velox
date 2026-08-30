package middleware

import (
	"context"
	"strings"
	"testing"
	"time"
)

// OpenRedis is the boot-time verdict input: "" is not an error (fail-open
// shape), a malformed URL is, and an unreachable server is — within the
// caller's ctx bound, never hanging boot.
func TestOpenRedis(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if c, err := OpenRedis(ctx, ""); c != nil || err != nil {
		t.Fatalf("empty url: want (nil, nil), got (%v, %v)", c, err)
	}
	if _, err := OpenRedis(ctx, "://not-a-url"); err == nil || !strings.Contains(err.Error(), "invalid REDIS_URL") {
		t.Fatalf("malformed url: want invalid REDIS_URL error, got %v", err)
	}
	start := time.Now()
	_, err := OpenRedis(ctx, "redis://127.0.0.1:1") // closed port
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("closed port: want unreachable error, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("unreachable Redis took %v — boot must not hang on the ping", time.Since(start))
	}
}
