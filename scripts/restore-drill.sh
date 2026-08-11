#!/usr/bin/env bash
# Velox Backup + Restore Drill
#
# Exercises the full backup → restore → validate loop against an ephemeral
# Postgres container. Run periodically (docs/ops/backup-considerations.md
# suggests quarterly) to verify the current backup pipeline actually
# restores; the timing log lets you track backup/restore duration against
# whatever RTO your deployment has committed to.
#
# What it does:
#   1. Captures source row counts for a list of critical tables.
#   2. Invokes scripts/backup.sh with a temporary BACKUP_DIR (no S3, no
#      rotation — the drill must not affect production backup infra).
#   3. Starts a one-shot Postgres container on a non-standard port.
#   4. Invokes scripts/restore.sh against the ephemeral container with
#      VELOX_RESTORE_CONFIRM=yes.
#   5. Re-reads the same critical-table counts and fails if any diverge.
#   6. Appends a pass/fail line to DRILL_LOG with timings so trend-over-time
#      on backup/restore duration is visible.
#   7. Tears the container down even on error.
#
# Usage:
#   SOURCE_DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" \
#     ./scripts/restore-drill.sh
#
# Optional environment variables:
#   SOURCE_DATABASE_URL    Source DB to back up and compare against. Falls
#                          back to DATABASE_URL for convenience.
#   DRILL_PG_IMAGE         Postgres image for the ephemeral target
#                          (default: postgres:16-alpine).
#   DRILL_PG_PORT          Host port exposed by the ephemeral container
#                          (default: 15432 — picked to avoid the usual 5432).
#   DRILL_LOG              Path to the drill history file (default:
#                          ~/.velox/drill.log). Auto-created.
#   CRITICAL_TABLES        Space-separated list of tables to count-match
#                          (default: tenants customers subscriptions
#                          invoices customer_credit_ledger).
#
# Exit codes:
#   0  Drill passed.
#   1  Configuration or environment error.
#   2  Backup step failed.
#   3  Restore step failed.
#   4  Row-count mismatch between source and restored DB.
#   5  Docker / ephemeral Postgres failed to come up.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

SOURCE_DATABASE_URL="${SOURCE_DATABASE_URL:-${DATABASE_URL:-}}"
# Default the ephemeral target to the SOURCE SERVER's major, so the drill
# rehearses the restore that matters: back into your own fleet's version.
# (A hardcoded 16 here hid nothing while backup.sh could still dump with
# newer host tools — the 2026-08-11 first drill failed exactly there.)
if [ -z "${DRILL_PG_IMAGE:-}" ] && command -v psql >/dev/null 2>&1; then
  _SRC_MAJOR=$(psql "${SOURCE_DATABASE_URL:-$DATABASE_URL}" -tAc "SHOW server_version" 2>/dev/null | sed -E 's/^([0-9]+).*/\1/')
  [ -n "$_SRC_MAJOR" ] && DRILL_PG_IMAGE="postgres:${_SRC_MAJOR}-alpine"
fi
DRILL_PG_IMAGE="${DRILL_PG_IMAGE:-postgres:16-alpine}"
# The restore into the ephemeral target must use tools matching ITS major —
# host pg_restore can be newer (see restore.sh guard). Feed a wrapped binary
# unless the operator overrode PG_RESTORE explicitly.
export PG_RESTORE="${PG_RESTORE:-docker run --rm -i --network host $DRILL_PG_IMAGE pg_restore}"
DRILL_PG_PORT="${DRILL_PG_PORT:-15432}"
DRILL_CONTAINER_NAME="${DRILL_CONTAINER_NAME:-velox-drill-$$}"
DRILL_LOG="${DRILL_LOG:-$HOME/.velox/drill.log}"
CRITICAL_TABLES="${CRITICAL_TABLES:-tenants customers subscriptions invoices customer_credit_ledger}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log() {
  echo "[$(date -u +"%Y-%m-%dT%H:%M:%SZ")] $*"
}

die() {
  log "FATAL: $*" >&2
  exit "${2:-1}"
}

cleanup() {
  local rc=$?
  if [ -n "${DRILL_CONTAINER_STARTED:-}" ]; then
    log "Cleaning up ephemeral container: $DRILL_CONTAINER_NAME"
    docker rm -f "$DRILL_CONTAINER_NAME" >/dev/null 2>&1 || true
  fi
  if [ -n "${BACKUP_WORKDIR:-}" ] && [ -d "$BACKUP_WORKDIR" ]; then
    log "Cleaning up backup workdir: $BACKUP_WORKDIR"
    rm -rf "$BACKUP_WORKDIR"
  fi
  exit $rc
}
trap cleanup EXIT
trap 'trap - EXIT; cleanup' INT TERM

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------

[ -z "$SOURCE_DATABASE_URL" ] && die "SOURCE_DATABASE_URL (or DATABASE_URL) is required." 1

command -v docker   >/dev/null 2>&1 || die "docker not found in PATH." 1
command -v pg_dump  >/dev/null 2>&1 || die "pg_dump not found in PATH." 1
command -v psql     >/dev/null 2>&1 || die "psql not found in PATH." 1

[ -x "$SCRIPT_DIR/backup.sh" ]  || die "backup.sh not executable: $SCRIPT_DIR/backup.sh" 1
[ -x "$SCRIPT_DIR/restore.sh" ] || die "restore.sh not executable: $SCRIPT_DIR/restore.sh" 1

# Fail fast if the target port is already in use — we won't clobber anything.
if (echo >/dev/tcp/127.0.0.1/"$DRILL_PG_PORT") 2>/dev/null; then
  die "Port ${DRILL_PG_PORT} is already in use. Set DRILL_PG_PORT to a free port." 1
fi

mkdir -p "$(dirname "$DRILL_LOG")" 2>/dev/null || true

# ---------------------------------------------------------------------------
# Capture source row counts
# ---------------------------------------------------------------------------

BACKUP_WORKDIR=$(mktemp -d "${TMPDIR:-/tmp}/velox-drill.XXXXXX")
SOURCE_COUNTS_FILE="$BACKUP_WORKDIR/source_counts.txt"

log "Capturing source row counts (${CRITICAL_TABLES})..."
for table in $CRITICAL_TABLES; do
  count=$(psql "$SOURCE_DATABASE_URL" -t -A -c "SELECT count(*) FROM \"$table\"" 2>/dev/null || echo "MISSING")
  # A table missing in the SOURCE is a hard error: source "MISSING" would
  # compare equal to a restored-side "MISSING" and vacuously pass step 5's
  # divergence check — the drill would green-light without validating it.
  if [ "$count" = "MISSING" ]; then
    die "Critical table '$table' not found in the source database — check CRITICAL_TABLES." 1
  fi
  echo "$table $count" >> "$SOURCE_COUNTS_FILE"
  log "  source.$table = $count"
done

# ---------------------------------------------------------------------------
# Step 1 — Backup
# ---------------------------------------------------------------------------

DRILL_START=$(date +%s)
BACKUP_START=$DRILL_START

log "Step 1/3: backup → $BACKUP_WORKDIR"
# Explicitly unset BACKUP_S3_BUCKET so a drill never triggers an S3 upload.
# BACKUP_RETENTION_DAYS=99999 prevents the backup script from deleting older
# dumps in the *workdir* (it only touches files under BACKUP_DIR, but be safe).
DATABASE_URL="$SOURCE_DATABASE_URL" \
  BACKUP_DIR="$BACKUP_WORKDIR" \
  BACKUP_S3_BUCKET="" \
  BACKUP_RETENTION_DAYS=99999 \
  "$SCRIPT_DIR/backup.sh" \
  2>&1 | while IFS= read -r line; do log "  backup: $line"; done || die "backup.sh exited non-zero." 2

BACKUP_FILE="$BACKUP_WORKDIR/latest.dump"
[ -f "$BACKUP_FILE" ] || die "backup.sh completed but no latest.dump produced." 2

BACKUP_DURATION=$(( $(date +%s) - BACKUP_START ))
log "Step 1/3 complete in ${BACKUP_DURATION}s."

# ---------------------------------------------------------------------------
# Step 2 — Ephemeral Postgres
# ---------------------------------------------------------------------------

log "Step 2/3: launching ephemeral Postgres ($DRILL_PG_IMAGE) on port $DRILL_PG_PORT"
docker run -d \
  --name "$DRILL_CONTAINER_NAME" \
  -e POSTGRES_DB=velox_drill \
  -e POSTGRES_USER=velox \
  -e POSTGRES_PASSWORD=velox \
  -p "${DRILL_PG_PORT}:5432" \
  "$DRILL_PG_IMAGE" >/dev/null || die "docker run failed." 5
DRILL_CONTAINER_STARTED=yes

# Wait up to 60s for readiness.
WAIT_DEADLINE=$(( $(date +%s) + 60 ))
READY=""
while [ "$(date +%s)" -lt "$WAIT_DEADLINE" ]; do
  if docker exec "$DRILL_CONTAINER_NAME" pg_isready -U velox -d velox_drill -q 2>/dev/null; then
    READY=yes
    break
  fi
  sleep 1
done
[ -n "$READY" ] || die "Ephemeral Postgres did not become ready within 60s." 5
log "Ephemeral Postgres is ready."

# Provision the Velox runtime role the archive's GRANTs reference — the same
# step the DR runbook prescribes on a fresh cluster (restore.sh refuses to
# run without it, so the drill rehearses the full documented procedure).
docker exec "$DRILL_CONTAINER_NAME" psql -U velox -d velox_drill -q -c \
  "CREATE ROLE velox_app LOGIN PASSWORD 'velox_app'" || die "Could not provision velox_app role in ephemeral cluster." 5

TARGET_URL="postgres://velox:velox@localhost:${DRILL_PG_PORT}/velox_drill?sslmode=disable"

# ---------------------------------------------------------------------------
# Step 3 — Restore
# ---------------------------------------------------------------------------

RESTORE_START=$(date +%s)
log "Step 3/3: restoring $BACKUP_FILE → ephemeral DB"
DATABASE_URL="$TARGET_URL" VELOX_RESTORE_CONFIRM=yes \
  "$SCRIPT_DIR/restore.sh" "$BACKUP_FILE" \
  2>&1 | while IFS= read -r line; do log "  restore: $line"; done || die "restore.sh exited non-zero." 3

RESTORE_DURATION=$(( $(date +%s) - RESTORE_START ))
log "Step 3/3 complete in ${RESTORE_DURATION}s."

# ---------------------------------------------------------------------------
# Validation
# ---------------------------------------------------------------------------

log "Validating per-table row counts against source..."
VALIDATION_FAILED=0
while IFS=' ' read -r table expected; do
  [ -z "$table" ] && continue
  actual=$(psql "$TARGET_URL" -t -A -c "SELECT count(*) FROM \"$table\"" 2>/dev/null || echo "MISSING")
  if [ "$expected" = "$actual" ]; then
    log "  OK     $table: $expected"
  else
    log "  FAIL   $table: expected=$expected actual=$actual"
    VALIDATION_FAILED=1
  fi
done < "$SOURCE_COUNTS_FILE"

TOTAL_DURATION=$(( $(date +%s) - DRILL_START ))

# ---------------------------------------------------------------------------
# Record + summary
# ---------------------------------------------------------------------------

if [ $VALIDATION_FAILED -eq 0 ]; then
  RESULT=PASS
else
  RESULT=FAIL
fi

# Strip query params from URL for the log line — we don't want credentials.
SOURCE_LABEL="${SOURCE_DATABASE_URL%%\?*}"
SOURCE_LABEL="${SOURCE_LABEL##*@}"   # trim user:pass@
{
  echo "$(date -u +"%Y-%m-%dT%H:%M:%SZ") $RESULT backup=${BACKUP_DURATION}s restore=${RESTORE_DURATION}s total=${TOTAL_DURATION}s source=$SOURCE_LABEL"
} >> "$DRILL_LOG" 2>/dev/null || true

log "========================================"
log "Drill Summary"
log "  Result:       $RESULT"
log "  Backup:       ${BACKUP_DURATION}s"
log "  Restore:      ${RESTORE_DURATION}s"
log "  Total:        ${TOTAL_DURATION}s"
log "  RTO:          no fixed target — compare Total against your own RTO commitment (docs/ops/backup-considerations.md)"
log "  Tables:       $CRITICAL_TABLES"
log "  History:      $DRILL_LOG"
log "========================================"

if [ $VALIDATION_FAILED -ne 0 ]; then
  exit 4
fi
exit 0
