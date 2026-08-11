#!/usr/bin/env bash
# Velox Database Backup Script
#
# Creates a logical backup using pg_dump, optionally uploads to S3,
# and rotates old local backups.
#
# Usage:
#   DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" ./scripts/backup.sh
#
# Optional environment variables:
#   BACKUP_DIR          Local directory for backups (default: /var/backups/velox)
#   BACKUP_S3_BUCKET    S3 bucket name for remote upload (skipped if unset)
#   BACKUP_S3_PREFIX    S3 key prefix (default: logical-backups)
#   BACKUP_RETENTION_DAYS  Days to keep local backups (default: 30)
#
# Exit codes:
#   0  Success
#   1  Configuration error
#   2  pg_dump failed
#   3  Backup verification failed
#   4  S3 upload failed

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

if [ -z "${DATABASE_URL:-}" ]; then
  echo "ERROR: DATABASE_URL environment variable is required." >&2
  echo "Example: DATABASE_URL='postgres://velox:velox@localhost:5432/velox?sslmode=disable'" >&2
  exit 1
fi

BACKUP_DIR="${BACKUP_DIR:-/var/backups/velox}"
BACKUP_S3_BUCKET="${BACKUP_S3_BUCKET:-}"
BACKUP_S3_PREFIX="${BACKUP_S3_PREFIX:-logical-backups}"
BACKUP_RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-30}"

# Extract database name from URL for the filename.
# Handles: postgres://user:pass@host:port/dbname?params
DB_NAME=$(echo "$DATABASE_URL" | sed -E 's|.*://[^/]*/([^?]*).*|\1|')
if [ -z "$DB_NAME" ]; then
  DB_NAME="velox"
fi

TIMESTAMP=$(date -u +"%Y%m%d_%H%M%S")
BACKUP_FILE="${BACKUP_DIR}/${DB_NAME}_${TIMESTAMP}.dump"
BACKUP_FILENAME="${DB_NAME}_${TIMESTAMP}.dump"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log() {
  echo "[$(date -u +"%Y-%m-%d %H:%M:%S UTC")] $*"
}

die() {
  log "FATAL: $*" >&2
  exit "${2:-1}"
}

cleanup_on_failure() {
  if [ -f "$BACKUP_FILE" ]; then
    rm -f "$BACKUP_FILE"
    log "Cleaned up partial backup file."
  fi
}

trap cleanup_on_failure ERR

# ---------------------------------------------------------------------------
# Preflight checks
# ---------------------------------------------------------------------------

# PG_DUMP lets an operator point at a version-matched binary when the host
# tools don't match the server — e.g. a containerized one:
#   PG_DUMP="docker run --rm -i --network host postgres:16-alpine pg_dump"
# (The archive is written via stdout, so a wrapped binary needs no mounts.)
PG_DUMP="${PG_DUMP:-pg_dump}"

command -v ${PG_DUMP%% *} >/dev/null 2>&1 || die "pg_dump not found in PATH." 1

mkdir -p "$BACKUP_DIR" || die "Cannot create backup directory: $BACKUP_DIR" 1

# A pg_dump whose MAJOR is newer than the server's produces archives the
# server's own major cannot restore (the 2026-08-11 restore drill caught
# exactly this: pg_dump 18 emitted SET transaction_timeout, a PG17+ GUC,
# and the postgres:16 restore rolled back). A backup you cannot restore
# into your own fleet is not a backup — fail loudly at dump time, when the
# operator can still fix it, not at restore time, when they cannot.
# Override (e.g. deliberately migrating majors via dump/restore):
# VELOX_BACKUP_TOOL_MISMATCH_OK=yes
TOOL_MAJOR=$($PG_DUMP --version | sed -E 's/.* ([0-9]+)(\.[0-9]+)?.*/\1/')
if command -v psql >/dev/null 2>&1; then
  SERVER_MAJOR=$(psql "$DATABASE_URL" -tAc "SHOW server_version" 2>/dev/null | sed -E 's/^([0-9]+).*/\1/')
  if [ -n "$SERVER_MAJOR" ] && [ -n "$TOOL_MAJOR" ] && [ "$TOOL_MAJOR" -gt "$SERVER_MAJOR" ] \
     && [ "${VELOX_BACKUP_TOOL_MISMATCH_OK:-no}" != "yes" ]; then
    die "pg_dump major ($TOOL_MAJOR) is newer than the server major ($SERVER_MAJOR): the archive may not restore into postgres $SERVER_MAJOR. Use matching tools (e.g. PG_DUMP=\"docker run --rm -i --network host postgres:${SERVER_MAJOR}-alpine pg_dump\") or set VELOX_BACKUP_TOOL_MISMATCH_OK=yes if the mismatch is deliberate." 1
  fi
fi

# Test database connectivity.
if command -v pg_isready >/dev/null 2>&1; then
  # Parse host and port from DATABASE_URL for pg_isready.
  DB_HOST=$(echo "$DATABASE_URL" | sed -E 's|.*@([^:/]*)[:/].*|\1|')
  DB_PORT=$(echo "$DATABASE_URL" | sed -E 's|.*:([0-9]+)/.*|\1|')
  pg_isready -h "${DB_HOST:-localhost}" -p "${DB_PORT:-5432}" -q || die "Database is not reachable." 1
fi

# ---------------------------------------------------------------------------
# Create backup
# ---------------------------------------------------------------------------

log "Starting backup of database '${DB_NAME}'..."
log "Destination: ${BACKUP_FILE}"

START_TIME=$(date +%s)

# Guarded so a pg_dump failure exits with the documented code 2 — the bare
# pipeline under `set -eo pipefail` would propagate pg_dump's own status (1),
# which the header reserves for configuration errors. The || suppresses the
# ERR trap, so clean up the partial file explicitly.
# The archive travels via STDOUT (not --file) so PG_DUMP can be a wrapped
# binary — docker, ssh — that has no view of the host filesystem.
$PG_DUMP \
  --dbname="$DATABASE_URL" \
  --format=custom \
  --compress=6 \
  --verbose \
  2> >(while IFS= read -r line; do log "  pg_dump: $line"; done) \
  > "$BACKUP_FILE" \
  || { cleanup_on_failure; die "pg_dump failed." 2; }

if [ ! -f "$BACKUP_FILE" ]; then
  die "pg_dump completed but backup file not found." 2
fi

BACKUP_SIZE=$(wc -c < "$BACKUP_FILE" | tr -d ' ')
if [ "$BACKUP_SIZE" -eq 0 ]; then
  rm -f "$BACKUP_FILE"
  die "Backup file is empty — pg_dump likely failed silently." 2
fi

END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

log "Backup complete: ${BACKUP_FILE} ($(numfmt --to=iec "$BACKUP_SIZE" 2>/dev/null || echo "${BACKUP_SIZE} bytes"), ${DURATION}s)"

# ---------------------------------------------------------------------------
# Verify backup
# ---------------------------------------------------------------------------

log "Verifying backup integrity..."

pg_restore --list "$BACKUP_FILE" > /dev/null 2>&1 || die "Backup verification failed — pg_restore cannot read the archive." 3

TABLE_COUNT=$(pg_restore --list "$BACKUP_FILE" 2>/dev/null | grep -c "TABLE " || true)
log "Verification passed: archive contains ${TABLE_COUNT} table entries."

# ---------------------------------------------------------------------------
# Upload to S3 (optional)
# ---------------------------------------------------------------------------

if [ -n "$BACKUP_S3_BUCKET" ]; then
  if command -v aws >/dev/null 2>&1; then
    S3_KEY="s3://${BACKUP_S3_BUCKET}/${BACKUP_S3_PREFIX}/${BACKUP_FILENAME}"
    log "Uploading to ${S3_KEY}..."

    aws s3 cp "$BACKUP_FILE" "$S3_KEY" --only-show-errors || die "S3 upload failed." 4

    log "S3 upload complete."
  else
    log "WARNING: BACKUP_S3_BUCKET is set but 'aws' CLI is not installed. Skipping S3 upload."
  fi
fi

# ---------------------------------------------------------------------------
# Rotate old backups
# ---------------------------------------------------------------------------

log "Rotating local backups older than ${BACKUP_RETENTION_DAYS} days..."

DELETED_COUNT=0
while IFS= read -r old_file; do
  rm -f "$old_file"
  DELETED_COUNT=$((DELETED_COUNT + 1))
  log "  Deleted: $(basename "$old_file")"
done < <(find "$BACKUP_DIR" -name "${DB_NAME}_*.dump" -type f -mtime +"$BACKUP_RETENTION_DAYS" 2>/dev/null || true)

log "Rotation complete: ${DELETED_COUNT} old backup(s) removed."

# ---------------------------------------------------------------------------
# Create a symlink to the latest backup for easy access
# ---------------------------------------------------------------------------

ln -sf "$BACKUP_FILE" "${BACKUP_DIR}/latest.dump"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

log "========================================"
log "Backup Summary"
log "  Database:  ${DB_NAME}"
log "  File:      ${BACKUP_FILE}"
log "  Size:      $(numfmt --to=iec "$BACKUP_SIZE" 2>/dev/null || echo "${BACKUP_SIZE} bytes")"
log "  Duration:  ${DURATION}s"
log "  Verified:  yes"
log "  S3 Upload: ${BACKUP_S3_BUCKET:+yes}${BACKUP_S3_BUCKET:-skipped}"
log "  Rotated:   ${DELETED_COUNT} old backup(s) removed"
log "========================================"

exit 0
