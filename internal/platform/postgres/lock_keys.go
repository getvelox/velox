package postgres

// Transaction-scoped advisory-lock keys. This is the only advisory lock
// left in the app after ADR-114 moved singleton-role leadership onto the
// leader_leases row:
//
//   - LockKeyBootstrap serializes first-boot tenant bootstrap
//     (pg_advisory_xact_lock inside the bootstrap transaction).
//
// Migrations take goose's own session lock (id 4097083626, ADR-125) on a
// dedicated 1-connection pool; that key is goose's, not listed here.
//
// Retired keys, do not reuse the numbers: the per-role session keys
// 76540001-76540005 and the topology-probe key 76540008 (an old binary in
// a mixed fleet still takes them for the duration of its tick), and the
// hybrid migration loop key 76540007 (v0.8.x migration runs take it).
const (
	LockKeyBootstrap int64 = 76540006
)
