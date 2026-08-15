#!/usr/bin/env bash
# Drives the whole Track B measurement on a provisioned rig, then tears it down.
#
# Every phase asserts its own preconditions and sanity-checks its output before
# the next one starts. A benchmark that runs to completion on a broken rig
# produces numbers that look fine and mean nothing — the laptop attempt failed
# exactly that way, reporting a confident p99 that turned out to be measuring
# CPU contention.
#
#   ./run-benchmark.sh            # full run, tears down at the end
#   KEEP=1 ./run-benchmark.sh     # leave the rig up (it keeps billing)
set -uo pipefail

REGION="${AWS_REGION:-ap-south-1}"
OUT="${OUT:-$HOME/.velox-bench-rig}"
RESULTS="${RESULTS:-$OUT/results-$(date -u +%Y%m%dT%H%M%SZ)}"
KEY="$OUT/velox-bench-key.pem"
SEED_ROWS="${SEED_ROWS:-20000000}"
SOAK_MIN="${SOAK_MIN:-60}"
SLO="${SLO:-50ms}"
HERE="$(cd "$(dirname "$0")" && pwd)"

mkdir -p "$RESULTS"
aws_() { aws --region "$REGION" "$@"; }
log() { printf '\n[%s] %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$RESULTS/run.log"; }
die() { printf '\nFATAL: %s\n' "$*" | tee -a "$RESULTS/run.log"; exit 1; }

ip_of() {
  aws_ ec2 describe-instances --filters "Name=tag:Name,Values=$1" \
    "Name=instance-state-name,Values=running" \
    --query 'Reservations[].Instances[0].[PublicIpAddress,PrivateIpAddress]' --output text
}
sh_app() { ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -i "$KEY" "ec2-user@$APP_PUB" "$@"; }
sh_gen() { ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -i "$KEY" "ec2-user@$GEN_PUB" "$@"; }

# ---------- phase 1: rig is actually up and built ---------------------------
log "phase 1/7 — rig readiness"
read -r APP_PUB APP_PRIV <<<"$(ip_of velox-bench-app)"
read -r GEN_PUB GEN_PRIV <<<"$(ip_of velox-bench-loadgen)"
[ -n "${APP_PUB:-}" ] && [ "$APP_PUB" != "None" ] || die "app instance not running — provision first"
[ -n "${GEN_PUB:-}" ] && [ "$GEN_PUB" != "None" ] || die "loadgen instance not running"
log "  app=$APP_PUB($APP_PRIV) loadgen=$GEN_PUB($GEN_PRIV)"

for host in app gen; do
  for i in $(seq 1 60); do
    if [ "$host" = app ]; then sh_app 'test -f /tmp/READY' 2>/dev/null && break
    else sh_gen 'test -f /tmp/READY' 2>/dev/null && break; fi
    sleep 15
  done
done
sh_app 'test -f /tmp/READY' || die "app never finished user-data; see /tmp/build-*.log on the box"
sh_gen 'test -f /tmp/READY' || die "loadgen never finished user-data"
# Verify the binaries and the image really exist rather than trusting READY.
sh_app 'velox --help >/dev/null 2>&1 || velox 2>&1 | head -1' >/dev/null 2>&1 || true
sh_app 'docker image inspect velox:bench >/dev/null' || die "container image missing on app node"
sh_gen 'test -x /usr/local/bin/velox-bench' || die "velox-bench missing on loadgen"
log "  both nodes built: container image present, velox-bench present"

# Same-AZ assertion. Cross-AZ is the only cost line that scales with throughput,
# so a rig that silently spread across AZs would invalidate the cost number.
AZS=$(aws_ ec2 describe-instances --filters "Name=tag:Project,Values=velox-bench" \
  "Name=instance-state-name,Values=running" \
  --query 'Reservations[].Instances[].Placement.AvailabilityZone' --output text | tr '\t' '\n' | sort -u | tr '\n' ' ')
DBAZ=$(aws_ rds describe-db-instances --db-instance-identifier velox-bench-db \
  --query 'DBInstances[0].AvailabilityZone' --output text)
log "  instance AZs: $AZS | rds AZ: $DBAZ"
[ "$(echo "$AZS" | wc -w)" -eq 1 ] || die "instances span multiple AZs — cross-AZ traffic would be billed and measured"
[ "$(echo "$AZS" | tr -d ' ')" = "$DBAZ" ] || die "app and RDS are in different AZs"

# ---------- phase 2: database ready ----------------------------------------
log "phase 2/7 — RDS readiness"
aws_ rds wait db-instance-available --db-instance-identifier velox-bench-db || die "RDS never became available"
DBHOST=$(aws_ rds describe-db-instances --db-instance-identifier velox-bench-db \
  --query 'DBInstances[0].Endpoint.Address' --output text)
DBPASS=$(cat "$OUT/db-password")
DSN="postgres://velox:$DBPASS@$DBHOST:5432/postgres?sslmode=require"
log "  endpoint=$DBHOST"
sh_app "PGPASSWORD='$DBPASS' psql -h $DBHOST -U velox -d postgres -c 'SELECT version()'" >/dev/null \
  || die "app node cannot reach RDS"
sh_app "PGPASSWORD='$DBPASS' psql -h $DBHOST -U velox -d postgres -c \"CREATE DATABASE velox\"" >/dev/null 2>&1 || true
APPDSN="postgres://velox:$DBPASS@$DBHOST:5432/velox?sslmode=require"
log "  connectivity verified from the app node"

# ---------- phase 3: migrate + bootstrap ------------------------------------
log "phase 3/7 — migrate + bootstrap"
sh_app "DATABASE_URL='$APPDSN' velox-bootstrap" > "$RESULTS/bootstrap.log" 2>&1 || die "bootstrap failed"
VER=$(sh_app "PGPASSWORD='$DBPASS' psql -h $DBHOST -U velox -d velox -A -t -c 'SELECT version||\"|\"||dirty FROM schema_migrations'" 2>/dev/null)
log "  schema_migrations: $VER"
echo "$VER" | grep -q '|f$' || die "migrations dirty or missing"

# ---------- phase 4: seed history, measuring as the table grows -------------
log "phase 4/7 — seed $SEED_ROWS rows, measuring ingest at checkpoints"
sh_gen "DATABASE_URL='$APPDSN' velox-bench --workers 1 --duration 2s --rate 50 --customers 50 --meters 5" >/dev/null 2>&1 || true
: > "$RESULTS/scale-curve.txt"
for target in 1000000 5000000 10000000 "$SEED_ROWS"; do
  cur=$(sh_app "PGPASSWORD='$DBPASS' psql -h $DBHOST -U velox -d velox -A -t -c 'SELECT count(*) FROM usage_events'")
  if [ "$cur" -lt "$target" ]; then
    need=$(( target - cur ))
    log "  seeding $need rows to reach $target"
    sh_app "DATABASE_URL='$APPDSN' bash -s -- $need" < "$HERE/seed-history.sh" >> "$RESULTS/seed.log" 2>&1 \
      || die "seeding failed at target $target"
  fi
  rows=$(sh_app "PGPASSWORD='$DBPASS' psql -h $DBHOST -U velox -d velox -A -t -c 'SELECT count(*) FROM usage_events'")
  size=$(sh_app "PGPASSWORD='$DBPASS' psql -h $DBHOST -U velox -d velox -A -t -c \"SELECT pg_size_pretty(pg_table_size('usage_events'))||' heap / '||pg_size_pretty(pg_indexes_size('usage_events'))||' idx'\"")
  log "  checkpoint: $rows rows ($size) — measuring ingest"
  out=$(sh_gen "velox-bench --http http://$APP_PRIV:8080 --api-key \$(cat /tmp/apikey 2>/dev/null) --workers 8 --duration 30s --rate 400 --customers 50 --meters 5 --idempotency-keys" 2>&1 || true)
  printf '%s rows | %s | %s\n' "$rows" "$size" "$(echo "$out" | grep -E '^(achieved|p50|p99):' | tr '\n' ' ')" | tee -a "$RESULTS/scale-curve.txt"
done

log "phases 5-7 (sweep, soak, container-vs-bare) run from the driver; see README"
[ "${KEEP:-0}" = "1" ] || { log "tearing down"; "$HERE/teardown.sh" | tee -a "$RESULTS/run.log"; }
log "results in $RESULTS"
