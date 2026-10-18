#!/usr/bin/env bash
# Writes internal/platform/migrate/baseline/00180_baseline.sql: the schema
# Velox v0.8.1's 178 migrations build, as one file that goose migration 180
# runs on an empty database (ADR-125).
#
# Nobody edits the output by hand. The script makes a fixed set of edits to
# pg_dump's output (R1-R8 below), and every guard stops it on anything it
# does not expect. To change the baseline, change this script and push: the
# gen-baseline workflow runs it on the PR and uploads the file.
#
#   scripts/gen-baseline.sh
#       Needs Docker and the network. Starts the pinned Postgres, runs the
#       pinned v0.8.1 image's `velox migrate` on it, checks the result and
#       dumps the schema.
#   GEN_BASELINE_DUMP=schema.sql scripts/gen-baseline.sh
#       Only makes the edits, to a `pg_dump --schema-only` output you have.
#
#   OUT   where to write (default internal/platform/migrate/baseline/00180_baseline.sql)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=ci/pinned-images.sh
. "$ROOT/scripts/ci/pinned-images.sh"
OUT="${OUT:-$ROOT/internal/platform/migrate/baseline/00180_baseline.sql}"
WORK="$(mktemp -d)"

fail() { echo "gen-baseline: $*" >&2; exit 1; }

PG=""
NET=""
cleanup() {
  [ -n "$PG" ] && docker rm -f "$PG" >/dev/null 2>&1 || true
  [ -n "$NET" ] && docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

if [ -n "${GEN_BASELINE_DUMP:-}" ]; then
  cp "$GEN_BASELINE_DUMP" "$WORK/raw.sql"
  SOURCE="pg_dump output in $(basename "$GEN_BASELINE_DUMP")"
else
  NET="velox-gen-baseline-$$"
  PG="velox-gen-baseline-pg-$$"
  docker network create "$NET" >/dev/null
  docker run -d --name "$PG" --network "$NET" \
    -e POSTGRES_USER=velox -e POSTGRES_PASSWORD=velox -e POSTGRES_DB=velox \
    "$POSTGRES16_IMAGE" >/dev/null
  # TCP, not the socket: the image's first-boot init server listens only on
  # the socket, so this waits for the real server.
  for _ in $(seq 1 60); do
    docker exec "$PG" pg_isready -q -h 127.0.0.1 -U velox -d velox && break
    sleep 1
  done
  docker exec "$PG" pg_isready -q -h 127.0.0.1 -U velox -d velox || fail "postgres did not start"
  psql_pg() { docker exec -i "$PG" psql -v ON_ERROR_STOP=1 -qAt -U velox -d velox "$@"; }

  # Setup N: the role the migrations grant to, and nothing else. No default
  # privileges and no schema grants, so the dump holds only the grants the
  # migrations made themselves.
  psql_pg -c "CREATE ROLE velox_app LOGIN" >/dev/null

  docker run --rm --network "$NET" \
    -e DATABASE_URL="postgres://velox:velox@$PG:5432/velox?sslmode=disable" \
    "$VELOX_V081_IMAGE" migrate

  # The state the baseline stands for: version 180, not dirty; no rows
  # but the five leader_leases rows 0174 seeds; no sequence values.
  got=$(psql_pg -c "SELECT version || ',' || dirty FROM schema_migrations")
  [ "$got" = "180,false" ] || fail "schema_migrations is '$got', want '180,false'"
  rows=$(psql_pg <<'SQL'
SELECT c.relname || ':' || (xpath('/row/n/text()',
         query_to_xml(format('SELECT count(*) AS n FROM public.%I', c.relname), false, true, '')))[1]::text
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND c.relname <> 'schema_migrations'
ORDER BY 1
SQL
)
  nonempty=$(printf '%s\n' "$rows" | grep -v ':0$' || true)
  [ "$nonempty" = "leader_leases:5" ] || fail "want rows only in leader_leases (5), got: ${nonempty:-none}"
  docker exec "$PG" pg_dump -U velox -d velox --data-only -T public.schema_migrations > "$WORK/data.sql"
  if grep -q 'setval' "$WORK/data.sql"; then fail "the data dump sets a sequence value; the baseline does not carry one"; fi

  docker exec "$PG" pg_dump -U velox -d velox --schema-only --no-owner --no-tablespaces \
    --no-publications --no-subscriptions --no-security-labels \
    -T public.schema_migrations > "$WORK/raw.sql"
  SOURCE="$VELOX_V081_IMAGE on $POSTGRES16_IMAGE"
fi

# R1  drop psql meta-commands (\restrict and \unrestrict carry a random key
#     from pg_dump 16.10 on).
# R2  drop the "Dumped from" and "Dumped by" lines.
# R3  header SETs become SET LOCAL: goose runs every file of one Up on one
#     connection, so a plain SET would leak into the files after 180.
#     client_encoding and transaction_timeout (PG17+) are dropped; any SET
#     not on the list stops the script.
# R4  search_path is set transaction-local.
# R5  drop COMMENT ON EXTENSION: only the extension's owner may run it, and
#     managed Postgres creates the extensions itself.
awk '
BEGIN {
  split("statement_timeout lock_timeout idle_in_transaction_session_timeout standard_conforming_strings check_function_bodies xmloption client_min_messages row_security default_tablespace default_table_access_method", names, " ")
  for (i in names) allowed[names[i]] = 1
}
/^\\/ { next }
/^-- Dumped (from database|by pg_dump) version / { next }
/^SET (client_encoding|transaction_timeout) = / { next }
/^SET [a-z_]+ = .*;$/ {
  if (!($2 in allowed)) { print "gen-baseline: unexpected top-level SET: " $0 > "/dev/stderr"; bad = 1 }
  sub(/^SET /, "SET LOCAL ")
  print
  next
}
/^SET / { print "gen-baseline: unexpected top-level SET: " $0 > "/dev/stderr"; bad = 1; next }
$0 == "SELECT pg_catalog.set_config('\''search_path'\'', '\'''\'', false);" {
  print "SELECT pg_catalog.set_config('\''search_path'\'', '\'''\'', true);"
  searchpath++
  next
}
/^COMMENT ON EXTENSION / { next }
{ print }
END {
  if (searchpath != 1) { print "gen-baseline: want exactly one search_path set_config line, found " searchpath > "/dev/stderr"; bad = 1 }
  exit bad
}
' "$WORK/raw.sql" > "$WORK/body.sql" || fail "the edits stopped on unexpected input (above)"

# Guards on the result.
grep -q '^CREATE TABLE public.tenants ' "$WORK/body.sql" || fail "no CREATE TABLE public.tenants: is this a Velox dump?"
if grep -n '^\\' "$WORK/body.sql"; then fail "a psql meta-command is left"; fi
if grep -n '^SET ' "$WORK/body.sql" | grep -v '^[0-9]*:SET LOCAL '; then fail "a SET that is not LOCAL is left"; fi
if grep -n 'set_config(.search_path., .., false)' "$WORK/body.sql"; then fail "search_path is still set for the session"; fi
if grep -n 'ALTER DEFAULT PRIVILEGES' "$WORK/body.sql"; then fail "the dump has default privileges: run it on setup N (no default privileges)"; fi
if grep -n -E '^(GRANT|REVOKE) .* ON SCHEMA ' "$WORK/body.sql"; then fail "the dump grants on a schema: run it on setup N (no schema grants)"; fi
if grep -n -E '^(GRANT|REVOKE) ' "$WORK/body.sql" | grep -v -E ' (TO|FROM) velox_app;$'; then fail "a grant names a role other than velox_app"; fi
if grep -n '^COMMENT ON EXTENSION' "$WORK/body.sql"; then fail "COMMENT ON EXTENSION is left"; fi

mkdir -p "$(dirname "$OUT")"
{
  # R8  the header.
  cat <<EOF
-- Velox's schema at migration 180, the last migration of Velox v0.8.1.
-- goose migration 180 (internal/platform/migrate/baseline.go) runs this
-- file on an empty database, in one transaction (ADR-125).
--
-- Generated by scripts/gen-baseline.sh from $SOURCE.
-- Do not edit: change the script and generate the file again.

EOF
  cat "$WORK/body.sql"
  # R6  0174's leader_leases seed, as written, schema-qualified: each token
  #     comes from clock_timestamp() at install time.
  # R7  0150's revoke, the only REVOKE in the 178 files. Without default
  #     privileges it changes nothing; with them (Compose, managed Postgres)
  #     it restores the access the history leaves on audit_log.
  cat <<'EOF'
--
-- Migration 0174's seed, as written.
--

INSERT INTO public.leader_leases (role, holder_token)
SELECT r, (extract(epoch FROM clock_timestamp()) * 1000)::bigint
FROM unnest(ARRAY['billing','dunning','webhook_outbox','email_outbox','webhook_delivery']) AS r;

--
-- Migration 0150's revoke, so installs whose velox_app has default
-- privileges end with the same access as the history gives them.
--

REVOKE UPDATE, DELETE, TRUNCATE ON TABLE public.audit_log FROM velox_app;
EOF
} > "$OUT"

echo "gen-baseline: wrote $OUT ($(wc -l < "$OUT" | tr -d ' ') lines) from $SOURCE"
