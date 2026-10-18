#!/usr/bin/env bash
# The migrations CI job (ADR-125). Its invariant: a new install of HEAD and
# an install upgraded from Velox v0.8.1 are identical.
#
# For each of three role setups it starts two Postgres containers:
#   A  roles, then the v0.8.1 image's `velox migrate` (the 178 old files),
#      then HEAD's `velox migrate` twice (migration 180 takes it over);
#   B  roles, then HEAD's `velox migrate` on the empty database (the
#      baseline).
# and compares their schema (owners and access rights included), their
# data and their applied versions.
#
#   compose  superuser velox; velox_app made by deploy/compose/postgres-init.sh
#   managed  velox_owner, not a superuser, owns the database; the three
#            extensions made first by a superuser (docs/ops/postgres-requirements.md)
#   citest   superuser velox; the velox_test database of docker/init.sql
#
# Then two more checks:
#   restore drill  back up a v0.8.1 database, let HEAD take it over, restore
#                  the backup over it with scripts/restore.sh: HEAD must
#                  refuse and change nothing.
#   Postgres 14    HEAD applies on postgres:14 and reports up to date.
#
# Needs Docker, psql and pg_restore on the host, and Go to build HEAD.
#   VELOX_BIN   a HEAD build of cmd/velox (default: built into a temp dir)
#   KEEP_OUT    a directory to keep the dumps in (CI uploads it on failure)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# shellcheck source=pinned-images.sh
. "$ROOT/scripts/ci/pinned-images.sh"
WORK="$(mktemp -d)"
OUT="${KEEP_OUT:-$WORK/out}"
mkdir -p "$OUT"
CONTAINERS=()

cleanup() {
  for c in "${CONTAINERS[@]}"; do docker rm -f "$c" >/dev/null 2>&1 || true; done
  rm -rf "$WORK"
}
trap cleanup EXIT

say() { printf '\n== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

if [ -z "${VELOX_BIN:-}" ]; then
  VELOX_BIN="$WORK/velox"
  (cd "$ROOT" && go build -o "$VELOX_BIN" ./cmd/velox)
fi

docker pull -q "$POSTGRES16_IMAGE" >/dev/null
docker pull -q "$POSTGRES14_IMAGE" >/dev/null
docker pull -q "$VELOX_V081_IMAGE" >/dev/null

# start_pg NAME PORT IMAGE SETUP: starts Postgres for one role setup on a
# host port. The roles exist when it returns. Callers add NAME to
# CONTAINERS first: this runs in the caller's shell, not a subshell.
start_pg() {
  local name=$1 port=$2 image=$3 setup=$4
  local args=(-d --name "$name" -p "127.0.0.1:$port:5432")
  case "$setup" in
    compose)
      args+=(-e POSTGRES_USER=velox -e POSTGRES_PASSWORD=velox -e POSTGRES_DB=velox
             -e VELOX_APP_DB_PASSWORD=velox-app-ci
             -v "$ROOT/deploy/compose/postgres-init.sh:/docker-entrypoint-initdb.d/00-init.sh:ro") ;;
    managed)
      args+=(-e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=postgres) ;;
    citest)
      args+=(-e POSTGRES_USER=velox -e POSTGRES_PASSWORD=velox -e POSTGRES_DB=velox) ;;
  esac
  docker run "${args[@]}" "$image" >/dev/null
  local user=velox
  [ "$setup" = managed ] && user=postgres
  for _ in $(seq 1 60); do
    docker exec "$name" pg_isready -q -h 127.0.0.1 -U "$user" >&2 && break
    sleep 1
  done
  docker exec "$name" pg_isready -q -h 127.0.0.1 -U "$user" || fail "$name did not start"
  case "$setup" in
    managed)
      docker exec -i "$name" psql -v ON_ERROR_STOP=1 -q -U postgres -d postgres >&2 <<'SQL'
CREATE ROLE velox_owner WITH LOGIN PASSWORD 'velox_owner' NOSUPERUSER CREATEROLE;
CREATE DATABASE velox OWNER velox_owner;
\connect velox
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;
SQL
      # docs/ops/postgres-requirements.md, "The two database users", run
      # as the admin user.
      docker exec -i "$name" psql -v ON_ERROR_STOP=1 -q -U velox_owner -d velox >&2 <<'SQL'
CREATE ROLE velox_app WITH LOGIN PASSWORD 'velox-app-ci';
GRANT ALL PRIVILEGES ON DATABASE velox TO velox_app;
GRANT ALL ON SCHEMA public TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO velox_app;
SQL
      ;;
    citest)
      docker exec -i "$name" psql -v ON_ERROR_STOP=1 -q -U velox -d velox < "$ROOT/docker/init.sql" >&2 ;;
  esac
}

# The database each setup migrates, and the role that migrates it.
dsn() {
  local setup=$1 port=$2
  case "$setup" in
    compose) echo "postgres://velox:velox@127.0.0.1:$port/velox?sslmode=disable" ;;
    managed) echo "postgres://velox_owner:velox_owner@127.0.0.1:$port/velox?sslmode=disable" ;;
    citest)  echo "postgres://velox:velox@127.0.0.1:$port/velox_test?sslmode=disable" ;;
  esac
}
dbname() { case "$1" in citest) echo velox_test ;; *) echo velox ;; esac; }
superuser() { case "$1" in managed) echo postgres ;; *) echo velox ;; esac; }

# q CONTAINER SETUP SQL: one value, as the superuser.
q() { docker exec -i "$1" psql -v ON_ERROR_STOP=1 -qAt -U "$(superuser "$2")" -d "$(dbname "$2")" -c "$3"; }

v081_migrate() { docker run --rm --network host -e DATABASE_URL="$1" "$VELOX_V081_IMAGE" migrate; }
head_velox() { local url=$1; shift; DATABASE_URL="$url" "$VELOX_BIN" "$@"; }

# dump CONTAINER SETUP KIND: the comparable dump, without the two version
# records and without the lines that differ between any two dumps.
dump() {
  local c=$1 setup=$2 kind=$3
  local flags=(--schema-only)
  [ "$kind" = data ] && flags=(--data-only --column-inserts)
  docker exec "$c" pg_dump -U "$(superuser "$setup")" -d "$(dbname "$setup")" "${flags[@]}" \
      -T public.schema_migrations -T public.goose_db_version \
    | grep -v -E '^\\(un)?restrict |^-- Dumped (from database|by pg_dump) version ' \
    | sed -E "s/^(INSERT INTO public\.leader_leases \([^)]*\) VALUES \('[a-z_]+', )[0-9]+,/\1<holder_token>,/"
}

versions() { q "$1" "$2" "SELECT string_agg(version_id::text, ',' ORDER BY version_id) FROM public.goose_db_version"; }

# 0174's seed takes each row's token from clock_timestamp(), so the five
# tokens can straddle a millisecond: they are checked for age, not equality.
check_leases() {
  local c=$1 setup=$2 got
  got=$(q "$c" "$setup" "SELECT count(*) || ',' ||
         bool_and(abs(holder_token - (extract(epoch FROM now()) * 1000)::bigint) < 86400000) FROM public.leader_leases")
  [ "$got" = "5,true" ] || fail "$c: leader_leases want 5 rows with tokens from the last day, got $got"
}

converge() {
  local setup=$1
  say "setup $setup"
  local a="mig-$setup-a" b="mig-$setup-b" pa=$2 pb=$3
  CONTAINERS+=("$a" "$b")
  start_pg "$a" "$pa" "$POSTGRES16_IMAGE" "$setup"
  start_pg "$b" "$pb" "$POSTGRES16_IMAGE" "$setup"
  local ua ub
  ua=$(dsn "$setup" "$pa"); ub=$(dsn "$setup" "$pb")

  echo "A: v0.8.1 migrate"
  v081_migrate "$ua"
  [ "$(q "$a" "$setup" "SELECT version || ',' || dirty FROM public.schema_migrations")" = "180,false" ] \
    || fail "$setup A: v0.8.1 did not end at (180, false)"

  echo "A: HEAD status before the move"
  head_velox "$ua" migrate status | tee "$OUT/$setup-A-status-before.txt"
  grep -q 'ready to move' "$OUT/$setup-A-status-before.txt" || fail "$setup A: status does not say ready to move"
  [ "$(q "$a" "$setup" "SELECT to_regclass('public.goose_db_version') IS NULL")" = t ] \
    || fail "$setup A: status created goose_db_version"

  echo "A: HEAD migrate, twice"
  head_velox "$ua" migrate 2>&1 | tee "$OUT/$setup-A-migrate-1.txt"
  head_velox "$ua" migrate 2>&1 | tee "$OUT/$setup-A-migrate-2.txt"
  grep -q 'migrations up to date' "$OUT/$setup-A-migrate-2.txt" || fail "$setup A: the second run did not say up to date"
  [ "$(q "$a" "$setup" "SELECT version || ',' || dirty FROM public.schema_migrations")" = "180,false" ] \
    || fail "$setup A: schema_migrations is no longer (180, false)"
  [ "$(q "$a" "$setup" "SELECT obj_description('public.schema_migrations'::regclass, 'pg_class')")" \
    = "velox: frozen at 180; the migration record moved to public.goose_db_version" ] \
    || fail "$setup A: schema_migrations does not carry the frozen-record comment"
  [ "$(q "$a" "$setup" "SELECT count(*) = count(DISTINCT version_id) AND bool_or(version_id = 0) AND bool_or(version_id = 180) FROM public.goose_db_version")" = t ] \
    || fail "$setup A: goose_db_version is not rows {0, 180, ...} with one row each"
  head_velox "$ua" migrate status | tee "$OUT/$setup-A-status-after.txt"
  grep -q 'up to date, nothing to apply' "$OUT/$setup-A-status-after.txt" || fail "$setup A: status after the move is not up to date"

  echo "B: HEAD migrate on an empty database"
  head_velox "$ub" migrate 2>&1 | tee "$OUT/$setup-B-migrate.txt"

  for kind in schema data; do
    dump "$a" "$setup" "$kind" > "$OUT/$setup-A-$kind.sql"
    dump "$b" "$setup" "$kind" > "$OUT/$setup-B-$kind.sql"
    if ! diff -u "$OUT/$setup-A-$kind.sql" "$OUT/$setup-B-$kind.sql" > "$OUT/$setup-$kind.diff"; then
      head -100 "$OUT/$setup-$kind.diff"
      fail "$setup: $kind differs between the upgraded install (A) and the new one (B)"
    fi
  done
  grep -q '^GRANT .* TO velox_app;$' "$OUT/$setup-A-schema.sql" || fail "$setup: the schema dump has no grants: access rights were not compared"
  if grep '^INSERT INTO' "$OUT/$setup-B-data.sql" | grep -v -q '^INSERT INTO public\.leader_leases '; then
    fail "$setup: a table other than leader_leases has rows"
  fi
  check_leases "$a" "$setup"
  check_leases "$b" "$setup"
  [ "$(versions "$a" "$setup")" = "$(versions "$b" "$setup")" ] \
    || fail "$setup: applied versions differ: A $(versions "$a" "$setup"), B $(versions "$b" "$setup")"
  echo "$setup: A and B are identical (versions $(versions "$a" "$setup"))"
}

restore_drill() {
  say "restore drill: a v0.8.1 backup restored over a database HEAD took over"
  local c=mig-restore port=55437 url before after out
  CONTAINERS+=("$c")
  start_pg "$c" "$port" "$POSTGRES16_IMAGE" compose
  url=$(dsn compose "$port")
  v081_migrate "$url"
  DATABASE_URL="$url" BACKUP_DIR="$WORK/backups" \
    PG_DUMP="docker run --rm -i --network host $POSTGRES16_IMAGE pg_dump" \
    "$ROOT/scripts/backup.sh"
  local backup
  backup=$(find "$WORK/backups" -type f -name 'velox_*.dump' | head -1) # not the latest.dump link
  head_velox "$url" migrate
  before=$(versions "$c" compose)
  DATABASE_URL="$url" VELOX_RESTORE_CONFIRM=yes \
    PG_RESTORE="docker run --rm -i --network host $POSTGRES16_IMAGE pg_restore" \
    "$ROOT/scripts/restore.sh" "$backup" | tee "$OUT/restore.txt"
  if out=$(head_velox "$url" migrate 2>&1); then
    echo "$out"
    fail "restore drill: HEAD migrate accepted a v0.8.x backup restored over a database it had taken over"
  fi
  echo "$out" | tee "$OUT/restore-migrate.txt"
  echo "$out" | grep -q "does not match this release's record in goose_db_version" \
    || fail "restore drill: HEAD migrate failed without the two-records message"
  after=$(versions "$c" compose)
  [ "$before" = "$after" ] || fail "restore drill: goose_db_version changed ($before -> $after)"
  head_velox "$url" migrate status | tee "$OUT/restore-status.txt"
  grep -q '^state: *refused' "$OUT/restore-status.txt" || fail "restore drill: status does not say refused"
  echo "restore drill: refused, nothing changed"
}

pg14() {
  say "Postgres 14: HEAD applies"
  local c=mig-pg14 port=55438 url
  CONTAINERS+=("$c")
  start_pg "$c" "$port" "$POSTGRES14_IMAGE" compose
  url=$(dsn compose "$port")
  head_velox "$url" migrate
  head_velox "$url" migrate status | tee "$OUT/pg14-status.txt"
  grep -q 'up to date, nothing to apply' "$OUT/pg14-status.txt" || fail "postgres 14: status is not up to date"
}

converge compose 55431 55432
converge managed 55433 55434
converge citest 55435 55436
restore_drill
pg14
say "migrations: all checks passed"
