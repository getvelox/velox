package migrate

import (
	"strings"
	"testing"
	"time"
)

func TestLockTimeoutFromEnv(t *testing.T) {
	cases := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"", DefaultLockTimeout, false},
		{"30s", 30 * time.Second, false},
		{" 2m ", 2 * time.Minute, false},
		{"0", 0, false},
		{"0s", 0, false},
		{"abc", 0, true},
		{"5", 0, true}, // a bare number is ambiguous: seconds or ms?
		{"-1s", 0, true},
		{"500us", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv(LockTimeoutEnv, tc.raw)
			got, err := lockTimeout()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), LockTimeoutEnv) {
				t.Fatalf("error does not name the setting: %v", err)
			}
		})
	}
}

// An invalid setting stops the migration before it connects, rather than
// silently running with no limit.
func TestOpenPool_RejectsBadLockTimeout(t *testing.T) {
	t.Setenv(LockTimeoutEnv, "soon")
	if _, err := openPool("postgres://u:p@127.0.0.1:1/x"); err == nil || !strings.Contains(err.Error(), LockTimeoutEnv) {
		t.Fatalf("want a %s error, got %v", LockTimeoutEnv, err)
	}
}
