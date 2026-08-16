#!/usr/bin/env bash
# Takes provisioned hardware to a RUNNING, SEEDED, VERIFIED velox — the step
# that did not exist between `provision.sh --yes` and `k6 run`.
#
# provision.sh installs, clones and builds, then stops. The benchmark doc used
# to jump straight from there to `k6 run -e BASE=http://<app>:8080`, and port
# 8080 answers nothing: nothing started the server, nothing migrated the
# database, nothing created the role the request path is supposed to use.
#
# Four things here are load-bearing, each for a reason the rig got wrong before:
#
#   1. ALTER DEFAULT PRIVILEGES runs BEFORE the migrations. Default privileges
#      apply to FUTURE objects only, so granting after the tables exist leaves
#      the app role unable to read most of them. `CREATE ROLE velox_app` alone
#      is necessary and NOT sufficient — skipping the default privileges
#      produces a cluster that migrates cleanly and then 500s across 11 tables.
#   2. APP_DATABASE_URL is set EXPLICITLY and ENV=production. With neither,
#      cmd/velox warns and falls back to the ADMIN pool (see openAppPool), so
#      the request path runs without RLS enforcement — a configuration nobody
#      self-hosts, and therefore not a number worth publishing.
#   3. The server runs as the CONTAINER by default, because that is what
#      deploy/compose runs and what a reader following our own docs will get.
#      APP_RUNTIME=binary measures the same code without the container layer.
#   4. It ends with a SMOKE TEST that ingests one event and confirms the row
#      landed. Every failure above is silent — a server that fell back to the
#      admin pool, or a seed that wrote nothing, still answers 200 on /health.
#
# MODE=local runs every step against a local Postgres so the logic itself can
# be tested without spending money. It is the same code path; only the shell
# that executes the app-side commands differs.
#
#   MODE=local  ADMIN_DSN=postgres://... ./bringup.sh
#   MODE=aws    ./bringup.sh                 # resolves the rig from AWS
set -euo pipefail

MODE="${MODE:-aws}"
REGION="${AWS_REGION:-ap-south-1}"
OUT="${OUT:-$HOME/.velox-bench-rig}"
DBNAME="${DBNAME:-velox}"
APP_RUNTIME="${APP_RUNTIME:-container}"
APP_PORT="${APP_PORT:-8080}"
LOG_LEVEL="${LOG_LEVEL:-warn}"   # a log line per request is real synchronous I/O
CREDS="${CREDS:-$OUT/bench-creds.json}"

say()  { printf '\n== %s\n' "$*"; }
info() { printf '   %s\n' "$*"; }
die()  { printf '\nFATAL: %s\n' "$*" >&2; exit 1; }

mkdir -p "$OUT"

# ---------------------------------------------------------------------------
# app_sh runs a command where the velox server lives. The ONLY difference
# between local and aws mode.
# ---------------------------------------------------------------------------
if [ "$MODE" = "aws" ]; then
  KEYFILE="$OUT/velox-bench-key.pem"
  [ -s "$KEYFILE" ] || die "no private key at $KEYFILE — provision.sh must have created it"
  aws_() { aws --region "$REGION" "$@"; }

  say "resolving the rig"
  APP_PUB=$(aws_ ec2 describe-instances --filters "Name=tag:Name,Values=velox-bench-app" \
    "Name=instance-state-name,Values=running" \
    --query 'Reservations[].Instances[0].PublicIpAddress' --output text)
  APP_PRIV=$(aws_ ec2 describe-instances --filters "Name=tag:Name,Values=velox-bench-app" \
    "Name=instance-state-name,Values=running" \
    --query 'Reservations[].Instances[0].PrivateIpAddress' --output text)
  [ -n "$APP_PUB" ] && [ "$APP_PUB" != "None" ] || die "app instance is not running — run provision.sh first"
  info "app: $APP_PUB (private $APP_PRIV)"

  app_sh() { ssh -o StrictHostKeyChecking=no -o ConnectTimeout=15 -i "$KEYFILE" "ec2-user@$APP_PUB" "$@"; }

  say "waiting for the app node to finish building"
  # READY is only created when EVERY user-data step succeeded; NOT_READY means
  # something failed and /tmp/failures.log says what. Neither existing yet means
  # cloud-init is still running.
  for _ in $(seq 1 80); do
    state=$(app_sh 'if [ -f /tmp/READY ]; then echo READY; elif [ -f /tmp/NOT_READY ]; then echo NOT_READY; else echo BUILDING; fi' 2>/dev/null || echo UNREACHABLE)
    [ "$state" = "READY" ] && break
    if [ "$state" = "NOT_READY" ]; then
      app_sh 'cat /tmp/failures.log' 2>/dev/null | sed 's/^/     /'
      die "app node failed to build (see the failures above, and /tmp/*.log on the box)"
    fi
    sleep 15
  done
  [ "$state" = "READY" ] || die "app node never became READY (last state: $state)"
  info "app node READY"

  say "waiting for RDS"
  aws_ rds wait db-instance-available --db-instance-identifier velox-bench-db
  DBHOST=$(aws_ rds describe-db-instances --db-instance-identifier velox-bench-db \
    --query 'DBInstances[0].Endpoint.Address' --output text)
  DBAZ=$(aws_ rds describe-db-instances --db-instance-identifier velox-bench-db \
    --query 'DBInstances[0].AvailabilityZone' --output text)
  INSTAZ=$(aws_ ec2 describe-instances --filters "Name=tag:Project,Values=velox-bench" \
    "Name=instance-state-name,Values=running" \
    --query 'Reservations[].Instances[].Placement.AvailabilityZone' --output text | tr '\t' '\n' | sort -u)
  info "rds: $DBHOST (AZ $DBAZ; instances in $INSTAZ)"
  # The same-AZ check provision.sh could not complete: RDS has no AZ until it
  # leaves 'creating', so this is the first moment it can actually be verified.
  [ "$DBAZ" = "$(echo "$INSTAZ" | tr -d ' ')" ] || \
    die "cross-AZ: instances in $INSTAZ, RDS in $DBAZ. Every DB round trip would be billed AND would raise the latency floor. Tear down and re-provision."
  DBPASS=$(cat "$OUT/db-password")
  ADMIN_DSN="postgres://velox:$DBPASS@$DBHOST:5432/$DBNAME?sslmode=require"
  ADMIN_DSN_BOOT="postgres://velox:$DBPASS@$DBHOST:5432/postgres?sslmode=require"
  BASE_URL="http://$APP_PRIV:$APP_PORT"
else
  [ -n "${ADMIN_DSN:-}" ] || die "MODE=local requires ADMIN_DSN"
  app_sh() { bash -c "$*"; }
  ADMIN_DSN_BOOT="${ADMIN_DSN_BOOT:-$ADMIN_DSN}"
  # DBNAME must name the SAME database ADMIN_DSN points at. In aws mode the DSN
  # is built from DBNAME so they agree by construction; here the caller supplies
  # both, and a mismatch meant the script checked whether database A existed and
  # then connected to database B.
  DBNAME=$(printf '%s' "$ADMIN_DSN" | sed -e 's|^.*/||' -e 's|?.*$||')
  [ -n "$DBNAME" ] || die "could not derive a database name from ADMIN_DSN"
  BASE_URL="http://localhost:$APP_PORT"
  info "local mode: $BASE_URL"
fi

APP_PASSWORD="${APP_PASSWORD:-$(openssl rand -hex 16)}"
APP_DSN=$(printf '%s' "$ADMIN_DSN" | sed "s|//velox:[^@]*@|//velox_app:$APP_PASSWORD@|")

psql_admin() { app_sh "psql '$ADMIN_DSN' -v ON_ERROR_STOP=1 -qtA -c \"$1\""; }

# ---------------------------------------------------------------------------
say "database + least-privilege role (BEFORE migrations)"
# createdb is not idempotent, so ask first.
exists=$(app_sh "psql '$ADMIN_DSN_BOOT' -qtA -c \"SELECT 1 FROM pg_database WHERE datname='$DBNAME'\"" || true)
if [ "$exists" != "1" ]; then
  app_sh "psql '$ADMIN_DSN_BOOT' -v ON_ERROR_STOP=1 -qtA -c \"CREATE DATABASE $DBNAME\""
  info "created database $DBNAME"
else
  info "database $DBNAME already exists"
fi

# Mirrors deploy/compose/postgres-init.sh — the shipped reference. Kept in the
# same order, because ALTER DEFAULT PRIVILEGES only affects objects created
# AFTER it runs, which is why it must precede `velox migrate`.
#
# The SQL goes over STDIN rather than inside the command string. A plpgsql
# DO $$...$$ block does not survive two layers of shell quoting (bash -c here,
# ssh there) — psql received a mangled body and answered "invalid command \$".
# CREATE ROLE is attempted and its "already exists" tolerated, which needs no
# $$ at all. APP_PASSWORD is hex from `openssl rand`, so inlining it introduces
# no quoting hazard.
ROLE_SQL=$(mktemp)
cat > "$ROLE_SQL" <<SQL
ALTER ROLE velox_app PASSWORD '$APP_PASSWORD';
GRANT ALL PRIVILEGES ON DATABASE $DBNAME TO velox_app;
GRANT ALL ON SCHEMA public TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO velox_app;
SQL
app_sh "psql '$ADMIN_DSN' -qtA -c 'CREATE ROLE velox_app WITH LOGIN'" >/dev/null 2>&1 || true
app_sh "psql '$ADMIN_DSN' -v ON_ERROR_STOP=1 -qtA -f -" < "$ROLE_SQL" >/dev/null
rm -f "$ROLE_SQL"
info "velox_app created with default privileges"

# ---------------------------------------------------------------------------
say "migrations"
app_sh "DATABASE_URL='$ADMIN_DSN' velox migrate" >/dev/null
ver=$(psql_admin "SELECT version || CASE WHEN dirty THEN ' DIRTY' ELSE '' END FROM schema_migrations")
info "schema_migrations: $ver"
case "$ver" in *DIRTY*) die "migrations are dirty — refusing to benchmark a half-migrated schema";; esac

# The trap the doc calls out: the role exists and the migration succeeded, and
# the app can still be unable to read most tables. Prove the grant took.
denied=$(app_sh "psql '$APP_DSN' -qtA -c 'SELECT count(*) FROM provider_cost_rates' 2>&1" || true)
case "$denied" in
  *"permission denied"*) die "velox_app cannot read provider_cost_rates — ALTER DEFAULT PRIVILEGES did not take effect before migration. Drop the database and re-run." ;;
esac
info "velox_app can read post-migration tables"

# ---------------------------------------------------------------------------
say "starting velox ($APP_RUNTIME)"
app_sh "pkill -f 'velox$' 2>/dev/null; docker rm -f velox-bench 2>/dev/null; true" >/dev/null 2>&1 || true
if [ "$APP_RUNTIME" = "container" ]; then
  app_sh "docker run -d --name velox-bench --network host \
    -e ENV=production -e PORT=$APP_PORT -e LOG_LEVEL=$LOG_LEVEL \
    -e DATABASE_URL='$ADMIN_DSN' -e APP_DATABASE_URL='$APP_DSN' \
    ${DB_MAX_OPEN_CONNS:+-e DB_MAX_OPEN_CONNS=$DB_MAX_OPEN_CONNS} \
    velox:bench" >/dev/null
else
  app_sh "ENV=production PORT=$APP_PORT LOG_LEVEL=$LOG_LEVEL \
    DATABASE_URL='$ADMIN_DSN' APP_DATABASE_URL='$APP_DSN' \
    ${DB_MAX_OPEN_CONNS:+DB_MAX_OPEN_CONNS=$DB_MAX_OPEN_CONNS} \
    nohup velox >/tmp/velox.log 2>&1 &" >/dev/null
fi

for _ in $(seq 1 40); do
  app_sh "curl -sf $BASE_URL/health >/dev/null" 2>/dev/null && break
  sleep 3
done
app_sh "curl -sf $BASE_URL/health >/dev/null" 2>/dev/null || {
  if [ "$APP_RUNTIME" = "container" ]; then app_sh "docker logs --tail 30 velox-bench" 2>&1 | sed 's/^/     /'
  else app_sh "tail -30 /tmp/velox.log" 2>&1 | sed 's/^/     /'; fi
  die "velox never became healthy on $BASE_URL"
}
info "velox healthy on $BASE_URL"

# ADR-073: an app pool that fell back to admin means the request path runs
# without RLS. The server logs it rather than refusing, so check for it.
if [ "$APP_RUNTIME" = "container" ]; then applog=$(app_sh "docker logs velox-bench 2>&1" || true)
else applog=$(app_sh "cat /tmp/velox.log" || true); fi
case "$applog" in
  *"falling back to admin"*) die "velox fell back to the ADMIN pool — RLS is not enforced on the request path, so this would measure a configuration nobody ships" ;;
esac
info "request path is running as velox_app (no admin fallback)"

# ---------------------------------------------------------------------------
say "seeding bench fixtures"
seed_json=$(app_sh "DATABASE_URL='$ADMIN_DSN' velox-bench-seed")
echo "$seed_json" > "$CREDS"; chmod 600 "$CREDS"
API_KEY=$(printf '%s' "$seed_json" | sed 's/.*"api_key":"\([^"]*\)".*/\1/')
CUSTOMER=$(printf '%s' "$seed_json" | sed 's/.*"external_customer_id":"\([^"]*\)".*/\1/')
EVENT=$(printf '%s' "$seed_json" | sed 's/.*"event_name":"\([^"]*\)".*/\1/')
[ -n "$API_KEY" ] || die "velox-bench-seed produced no api_key"
info "fixtures ready; credentials in $CREDS"

# ---------------------------------------------------------------------------
say "smoke test — ingest one event and prove the row landed"
before=$(psql_admin "SELECT count(*) FROM usage_events WHERE tenant_id='vlx_ten_bench'")
code=$(app_sh "curl -s -o /dev/null -w '%{http_code}' -X POST $BASE_URL/v1/usage-events \
  -H 'Authorization: Bearer $API_KEY' -H 'Content-Type: application/json' \
  -d '{\"external_customer_id\":\"$CUSTOMER\",\"event_name\":\"$EVENT\",\"quantity\":1,\"idempotency_key\":\"bringup-smoke-'\$(date +%s)'\"}'")
after=$(psql_admin "SELECT count(*) FROM usage_events WHERE tenant_id='vlx_ten_bench'")
[ "$code" = "201" ] || die "ingest returned HTTP $code, expected 201"
[ "$after" -gt "$before" ] || die "ingest returned 201 but NO ROW was written ($before -> $after) — the 2xx is not evidence"
info "HTTP $code and the row landed ($before -> $after)"

say "READY TO MEASURE"
cat <<EOF
   base url : $BASE_URL
   creds    : $CREDS
   run k6 (from the loadgen node in aws mode):

     k6 run -e BASE=$BASE_URL -e API_KEY=\$(sed 's/.*"api_key":"\([^"]*\)".*/\1/' $CREDS) \\
            -e RATE=1000 -e BATCH=10 -e DURATION=10m \\
            /opt/velox/scripts/bench-rig/ingest.js

   the rig is billing — tear down with ./teardown.sh when finished
EOF
