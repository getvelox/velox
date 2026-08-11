#!/usr/bin/env bash
# Velox Database Restore Script
#
# Restores a database from a pg_dump backup file.
#
# Usage:
#   DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" \
#     ./scripts/restore.sh /var/backups/velox/velox_20260415_030000.dump
#
# Optional environment variables:
#   VELOX_RESTORE_CONFIRM=yes   Skip the interactive confirmation prompt (for automation).
#   VELOX_MIGRATIONS_DIR        Path to migrations directory (default: auto-detected).
#
# Exit codes:
#   0  Success
#   1  Configuration/argument error
#   2  Restore failed
#   3  Post-restore validation failed

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

BACKUP_FILE="${1:-}"

if [ -z "$BACKUP_FILE" ]; then
  echo "Usage: ./scripts/restore.sh <backup-file>"
  echo ""
  echo "Examples:"
  echo "  ./scripts/restore.sh /var/backups/velox/velox_20260415_030000.dump"
  echo "  ./scripts/restore.sh /var/backups/velox/latest.dump"
  echo ""
  echo "Environment:"
  echo "  DATABASE_URL              Required. Target database connection string."
  echo "  VELOX_RESTORE_CONFIRM=yes  Skip interactive confirmation (for CI/automation)."
  exit 1
fi

if [ -z "${DATABASE_URL:-}" ]; then
  echo "ERROR: DATABASE_URL environment variable is required." >&2
  exit 1
fi

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

# ---------------------------------------------------------------------------
# Preflight checks
# ---------------------------------------------------------------------------

# Resolve symlinks so we display the actual file.
if [ -L "$BACKUP_FILE" ]; then
  RESOLVED_FILE=$(readlink -f "$BACKUP_FILE" 2>/dev/null || readlink "$BACKUP_FILE")
  log "Resolved symlink: $BACKUP_FILE -> $RESOLVED_FILE"
  BACKUP_FILE="$RESOLVED_FILE"
fi

if [ ! -f "$BACKUP_FILE" ]; then
  die "Backup file not found: $BACKUP_FILE" 1
fi

BACKUP_SIZE=$(wc -c < "$BACKUP_FILE" | tr -d ' ')
if [ "$BACKUP_SIZE" -eq 0 ]; then
  die "Backup file is empty: $BACKUP_FILE" 1
fi

# Detect file format.
FILE_TYPE="unknown"
if file "$BACKUP_FILE" | grep -q "PostgreSQL custom database dump"; then
  FILE_TYPE="custom"
elif file "$BACKUP_FILE" | grep -q "ASCII\|UTF-8\|text"; then
  FILE_TYPE="sql"
else
  # Try pg_restore --list as a heuristic for custom format.
  if pg_restore --list "$BACKUP_FILE" >/dev/null 2>&1; then
    FILE_TYPE="custom"
  fi
fi

log "Backup file: $BACKUP_FILE"
log "File size:   $(numfmt --to=iec "$BACKUP_SIZE" 2>/dev/null || echo "${BACKUP_SIZE} bytes")"
log "File type:   $FILE_TYPE"

# Extract target database info for display.
DB_HOST=$(echo "$DATABASE_URL" | sed -E 's|.*@([^:/]*)[:/].*|\1|')
DB_PORT=$(echo "$DATABASE_URL" | sed -E 's|.*:([0-9]+)/.*|\1|')
DB_NAME=$(echo "$DATABASE_URL" | sed -E 's|.*://[^/]*/([^?]*).*|\1|')

# Check required tools. PG_RESTORE lets an operator point at a
# version-matched binary when host tools don't match the target server —
# e.g. PG_RESTORE="docker run --rm -i --network host postgres:16-alpine pg_restore"
# (the archive is fed via stdin, so a wrapped binary needs no mounts).
PG_RESTORE="${PG_RESTORE:-pg_restore}"
command -v ${PG_RESTORE%% *} >/dev/null 2>&1 || die "pg_restore not found in PATH." 1
command -v psql >/dev/null 2>&1 || die "psql not found in PATH." 1

# pg_restore emits client-version SET commands (SET transaction_timeout is
# PG17+) regardless of the archive's origin, so a pg_restore major newer
# than the TARGET server rolls the whole single-transaction restore back.
# The 2026-08-11 drill hit exactly this. Fail before touching the target.
RESTORE_TOOL_MAJOR=$($PG_RESTORE --version | sed -E 's/.* ([0-9]+)(\.[0-9]+)?.*/\1/')
TARGET_MAJOR=$(psql "$DATABASE_URL" -tAc "SHOW server_version" 2>/dev/null | sed -E 's/^([0-9]+).*/\1/')
if [ -n "$TARGET_MAJOR" ] && [ -n "$RESTORE_TOOL_MAJOR" ] && [ "$RESTORE_TOOL_MAJOR" -gt "$TARGET_MAJOR" ] \
   && [ "${VELOX_RESTORE_TOOL_MISMATCH_OK:-no}" != "yes" ]; then
  die "pg_restore major ($RESTORE_TOOL_MAJOR) is newer than the target server major ($TARGET_MAJOR) — its client SET commands will roll the restore back. Use matching tools (e.g. PG_RESTORE=\"docker run --rm -i --network host postgres:${TARGET_MAJOR}-alpine pg_restore\") or set VELOX_RESTORE_TOOL_MISMATCH_OK=yes." 1
fi

# Velox archives carry GRANTs to the velox_app runtime role (ADR-073). On a
# FRESH target — the disaster-recovery case — that role does not exist, one
# GRANT fails, and --single-transaction rolls the whole restore back. Roles
# are cluster-level, so pg_dump of a database cannot carry them; the target
# must provision them first. Fail before touching anything, with the remedy.
if ! psql "$DATABASE_URL" -tAc "SELECT 1 FROM pg_roles WHERE rolname='velox_app'" 2>/dev/null | grep -q 1; then
  die "Target cluster has no 'velox_app' role — the archive's GRANTs will roll the restore back. Create it first (CREATE ROLE velox_app LOGIN PASSWORD '<your-password>'; see docker/init.sql for the dev shape and docs/ops/backup-considerations.md for the restore runbook), then re-run." 1
fi

# Test database connectivity.
if command -v pg_isready >/dev/null 2>&1; then
  pg_isready -h "${DB_HOST:-localhost}" -p "${DB_PORT:-5432}" -q || die "Database is not reachable at ${DB_HOST}:${DB_PORT}." 1
fi

# ---------------------------------------------------------------------------
# Confirmation
# ---------------------------------------------------------------------------

echo ""
echo "================================================================"
echo "  WARNING: This will OVERWRITE data in the target database."
echo ""
echo "  Target:  ${DB_NAME} @ ${DB_HOST}:${DB_PORT}"
echo "  Source:   ${BACKUP_FILE}"
echo "  Size:    $(numfmt --to=iec "$BACKUP_SIZE" 2>/dev/null || echo "${BACKUP_SIZE} bytes")"
echo "================================================================"
echo ""

if [ "${VELOX_RESTORE_CONFIRM:-}" = "yes" ]; then
  log "VELOX_RESTORE_CONFIRM=yes — skipping interactive prompt."
else
  printf "Type 'yes' to proceed: "
  read -r CONFIRM
  if [ "$CONFIRM" != "yes" ]; then
    log "Restore cancelled by operator."
    exit 0
  fi
fi

# ---------------------------------------------------------------------------
# Pre-restore: capture current state
# ---------------------------------------------------------------------------

log "Capturing pre-restore row counts for comparison..."

PRE_COUNTS=$(psql "$DATABASE_URL" -t -A -c "
  SELECT coalesce(sum(n_live_tup), 0)
  FROM pg_stat_user_tables;
" 2>/dev/null || echo "unknown")

log "Pre-restore total rows: ${PRE_COUNTS}"

# ---------------------------------------------------------------------------
# Restore
# ---------------------------------------------------------------------------

START_TIME=$(date +%s)

if [ "$FILE_TYPE" = "custom" ]; then
  log "Restoring using pg_restore (custom format)..."

  # --clean drops existing objects before recreating.
  # --if-exists avoids errors when objects don't exist.
  # --no-owner skips ownership commands (the restoring user becomes owner).
  # --single-transaction ensures atomic restore — all or nothing.
  # Archive via STDIN (custom format supports it) so PG_RESTORE can be a
  # wrapped binary with no view of the host filesystem.
  $PG_RESTORE \
    --dbname="$DATABASE_URL" \
    --clean \
    --if-exists \
    --no-owner \
    --single-transaction \
    --verbose \
    < "$BACKUP_FILE" \
    2>&1 | while IFS= read -r line; do log "  pg_restore: $line"; done \
    || die "pg_restore failed — the single-transaction restore was rolled back." 2

elif [ "$FILE_TYPE" = "sql" ]; then
  log "Restoring using psql (SQL format)..."

  psql "$DATABASE_URL" \
    --single-transaction \
    --set ON_ERROR_STOP=on \
    -f "$BACKUP_FILE" \
    2>&1 | while IFS= read -r line; do log "  psql: $line"; done \
    || die "psql restore failed — the single-transaction restore was rolled back." 2

else
  # Input problem, not a failed restore — exit 1 per the header's taxonomy.
  die "Unrecognized backup file format. Expected pg_dump custom format or plain SQL." 1
fi

END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

log "Restore completed in ${DURATION}s."

# ---------------------------------------------------------------------------
# Post-restore validation
# ---------------------------------------------------------------------------

log "Running post-restore validation..."

# Check that critical tables exist and have data.
VALIDATION_QUERY="
  SELECT relname AS table_name, n_live_tup AS row_count
  FROM pg_stat_user_tables
  WHERE schemaname = 'public'
  ORDER BY n_live_tup DESC
  LIMIT 20;
"

log "Table row counts after restore:"
psql "$DATABASE_URL" -c "$VALIDATION_QUERY" 2>&1 | while IFS= read -r line; do log "  $line"; done

POST_COUNTS=$(psql "$DATABASE_URL" -t -A -c "
  SELECT coalesce(sum(n_live_tup), 0)
  FROM pg_stat_user_tables;
" 2>/dev/null || echo "unknown")

log "Post-restore total rows: ${POST_COUNTS}"

# Check critical Velox tables exist.
CRITICAL_TABLES="tenants customers subscriptions invoices customer_credit_ledger"
MISSING_TABLES=""

for table in $CRITICAL_TABLES; do
  EXISTS=$(psql "$DATABASE_URL" -t -A -c "
    SELECT EXISTS (
      SELECT 1 FROM information_schema.tables
      WHERE table_schema = 'public' AND table_name = '$table'
    );
  " 2>/dev/null || echo "f")

  if [ "$EXISTS" != "t" ]; then
    MISSING_TABLES="${MISSING_TABLES} ${table}"
  fi
done

if [ -n "$MISSING_TABLES" ]; then
  log "WARNING: The following critical tables are missing after restore:${MISSING_TABLES}"
  log "The restore may be incomplete. Check the backup file and try again."
  exit 3
fi

log "All critical tables present."

# ---------------------------------------------------------------------------
# Migration check
# ---------------------------------------------------------------------------

log "Checking migration state..."

# Look for a schema_migrations or goose_db_version table.
MIGRATION_TABLE=$(psql "$DATABASE_URL" -t -A -c "
  SELECT table_name FROM information_schema.tables
  WHERE table_schema = 'public'
    AND table_name IN ('schema_migrations', 'goose_db_version')
  LIMIT 1;
" 2>/dev/null || echo "")

if [ -n "$MIGRATION_TABLE" ]; then
  log "Migration table found: ${MIGRATION_TABLE}"

  if [ "$MIGRATION_TABLE" = "schema_migrations" ]; then
    LATEST_MIGRATION=$(psql "$DATABASE_URL" -t -A -c "
      SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1;
    " 2>/dev/null || echo "unknown")
    log "Latest applied migration: ${LATEST_MIGRATION}"
  elif [ "$MIGRATION_TABLE" = "goose_db_version" ]; then
    LATEST_MIGRATION=$(psql "$DATABASE_URL" -t -A -c "
      SELECT version_id FROM goose_db_version WHERE is_applied = true ORDER BY version_id DESC LIMIT 1;
    " 2>/dev/null || echo "unknown")
    log "Latest applied migration: ${LATEST_MIGRATION}"
  fi

  log "NOTE: If the application has newer migrations, run them after restore:"
  log "  DATABASE_URL=\"$DATABASE_URL\" RUN_MIGRATIONS_ON_BOOT=true go run ./cmd/velox"
  log "  Or start the application normally — it runs migrations on boot when RUN_MIGRATIONS_ON_BOOT=true."
else
  log "WARNING: No migration tracking table found. Migrations may need to be re-applied."
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

echo ""
log "========================================"
log "Restore Summary"
log "  Source:    ${BACKUP_FILE}"
log "  Target:    ${DB_NAME} @ ${DB_HOST}:${DB_PORT}"
log "  Duration:  ${DURATION}s"
log "  Rows:      ${PRE_COUNTS} (before) -> ${POST_COUNTS} (after)"
log "  Tables:    All critical tables present"
log "  Migration: ${LATEST_MIGRATION:-check manually}"
log "========================================"
echo ""
log "Restore complete. Verify application behavior before resuming traffic."

exit 0
