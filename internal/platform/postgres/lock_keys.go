package postgres

// Transaction-scoped advisory-lock keys. These two are the only advisory
// locks left in the app after ADR-114 moved singleton-role leadership onto
// the leader_leases row; both are held for the duration of ONE transaction
// or one dedicated direct connection, never across a pooled session:
//
//   - LockKeyBootstrap serializes first-boot tenant bootstrap
//     (pg_advisory_xact_lock inside the bootstrap transaction).
//   - LockKeyMigrateHybrid serializes the hybrid migration loop across
//     replicas (internal/platform/migrate, on a dedicated 1-connection pool —
//     migrations run as a deploy step on a direct or session-mode
//     connection; docs/ops/postgres-requirements.md).
//
// The former per-role session keys 76540001-76540005 and the topology-probe
// key 76540008 are retired; do not reuse the numbers — an old binary in a
// mixed fleet still takes them for the duration of its tick.
const (
	LockKeyBootstrap     int64 = 76540006
	LockKeyMigrateHybrid int64 = 76540007
)
