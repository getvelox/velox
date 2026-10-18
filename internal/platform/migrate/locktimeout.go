package migrate

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// LockTimeoutEnv names the setting that bounds how long a migration waits
// for a table lock (issue #987). A migration that changes a table needs a
// lock on it, and while it waits for that lock every other query on the
// table queues behind it. Without a limit, one long transaction in the
// app turns an upgrade into an outage of that table.
const LockTimeoutEnv = "MIGRATION_LOCK_TIMEOUT"

// DefaultLockTimeout is how long a migration waits for a lock when
// LockTimeoutEnv is unset. Long enough to get past ordinary short
// transactions, short enough that queued app queries recover quickly.
const DefaultLockTimeout = 5 * time.Second

// lockTimeout reads LockTimeoutEnv. "0" means no limit (Postgres'
// default), for an operator who has chosen to wait.
func lockTimeout() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(LockTimeoutEnv))
	if raw == "" {
		return DefaultLockTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if raw == "0" {
		d, err = 0, nil
	}
	if err != nil || d < 0 || (d > 0 && d < time.Millisecond) {
		return 0, fmt.Errorf("%s=%q: want a duration such as 5s, 30s or 2m, or 0 for no limit", LockTimeoutEnv, raw)
	}
	return d, nil
}
