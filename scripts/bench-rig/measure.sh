#!/usr/bin/env bash
# Runs the measurement PROTOCOL, not a single k6 invocation.
#
# Every figure published so far was n=1: one run, one number, no warmup
# discipline, and the event count taken from the load generator's own counter.
# For a billing engine that is the wrong standard in two separate ways —
# a single sample cannot separate signal from noise, and a client-side counter
# is not evidence that any row exists.
#
# Four things a defensible number needs that a bare `k6 run` does not give you:
#
#   1. WARMUP, discarded. Cold caches, an unsettled autovacuum and a JIT-cold
#      connection pool otherwise land inside the measurement.
#   2. REPEATS, so variance is visible. A median of N with the spread printed
#      beside it is a number someone can argue with; a single value is not.
#   3. RECONCILIATION. The database is asked how many rows actually landed, and
#      the run FAILS if that disagrees with what the client claimed. This is
#      the correctness half of a throughput claim, and for a system whose whole
#      promise is not losing or duplicating money events it is the half that
#      matters more. It has only ever been checked by hand.
#   4. ARTIFACTS pulled off the load generator BEFORE teardown destroys the box.
#
# It exits non-zero if any run fails to reconcile, so "fast but lossy" cannot
# be published by accident.
#
#   MODE=local ADMIN_DSN=postgres://... ./measure.sh
#   MODE=aws   ./measure.sh
#
# CONFIGS is a space-separated list of label:RATE:BATCH triples.
set -uo pipefail

MODE="${MODE:-aws}"
REGION="${AWS_REGION:-ap-south-1}"
OUT="${OUT:-$HOME/.velox-bench-rig}"
CREDS="${CREDS:-$OUT/bench-creds.json}"
REPEATS="${REPEATS:-3}"
WARMUP="${WARMUP:-30s}"
DURATION="${DURATION:-90s}"
PROBE_RATE="${PROBE_RATE:-0}"
PROBE_P99_MS="${PROBE_P99_MS:-500}"
CONFIGS="${CONFIGS:-single:200:1 batched:1000:10}"
RESULTS="${RESULTS:-$OUT/results-$(date -u +%Y%m%dT%H%M%SZ)}"
SCRIPT_PATH="${SCRIPT_PATH:-}"

say()  { printf '\n== %s\n' "$*"; }
info() { printf '   %s\n' "$*"; }
die()  { printf '\nFATAL: %s\n' "$*" >&2; exit 1; }

[ -s "$CREDS" ] || die "no credentials at $CREDS — run ./bringup.sh first"
mkdir -p "$RESULTS"

jqf() { sed 's/.*"'"$1"'":"\([^"]*\)".*/\1/' "$CREDS"; }
API_KEY=$(jqf api_key); CUSTOMER_ID=$(jqf customer_id)

HERE="$(cd "$(dirname "$0")" && pwd)"

if [ "$MODE" = "aws" ]; then
  KEYFILE="$OUT/velox-bench-key.pem"
  aws_() { aws --region "$REGION" "$@"; }
  ip_of() { aws_ ec2 describe-instances --filters "Name=tag:Name,Values=$1" \
      "Name=instance-state-name,Values=running" \
      --query "Reservations[].Instances[0].$2" --output text; }
  APP_PRIV=$(ip_of velox-bench-app PrivateIpAddress)
  GEN_PUB=$(ip_of velox-bench-loadgen PublicIpAddress)
  APP_PUB=$(ip_of velox-bench-app PublicIpAddress)
  [ -n "$GEN_PUB" ] && [ "$GEN_PUB" != "None" ] || die "loadgen instance is not running"
  app_sh() { ssh -o StrictHostKeyChecking=no -o ConnectTimeout=15 -i "$KEYFILE" "ec2-user@$APP_PUB" "$@"; }
  gen_sh() { ssh -o StrictHostKeyChecking=no -o ConnectTimeout=15 -i "$KEYFILE" "ec2-user@$GEN_PUB" "$@"; }
  BASE="http://$APP_PRIV:8080"
  SCRIPT_PATH="${SCRIPT_PATH:-/opt/velox/scripts/bench-rig/ingest.js}"
  DBPASS=$(cat "$OUT/db-password")
  DBHOST=$(aws_ rds describe-db-instances --db-instance-identifier velox-bench-db \
    --query 'DBInstances[0].Endpoint.Address' --output text)
  ADMIN_DSN="postgres://velox:$DBPASS@$DBHOST:5432/${DBNAME:-velox}?sslmode=require"
else
  [ -n "${ADMIN_DSN:-}" ] || die "MODE=local requires ADMIN_DSN"
  app_sh() { bash -c "$*"; }
  gen_sh() { bash -c "$*"; }
  BASE="${BASE:-http://localhost:8099}"
  SCRIPT_PATH="${SCRIPT_PATH:-$HERE/ingest.js}"
fi

rows() { app_sh "psql '$ADMIN_DSN' -qtA -c \"SELECT count(*) FROM usage_events WHERE tenant_id='vlx_ten_bench'\"" | tr -d '[:space:]'; }

# One k6 invocation. Echoes: events_claimed rows_written ev_per_sec p50 p99 dropped
one_run() {
  local rate=$1 batch=$2 dur=$3 tag=$4
  local before after out claimed evs p50 p99 dropped
  before=$(rows)
  gen_sh "k6 run --quiet \
    -e BASE=$BASE -e API_KEY=$API_KEY -e CUSTOMER_ID=$CUSTOMER_ID \
    -e RATE=$rate -e BATCH=$batch -e DURATION=$dur \
    -e PROBE_RATE=$PROBE_RATE -e PROBE_P99_MS=$PROBE_P99_MS \
    $SCRIPT_PATH" > "$RESULTS/$tag.txt" 2>&1
  local rc=$?
  after=$(rows)
  out=$(cat "$RESULTS/$tag.txt")
  claimed=$(printf '%s' "$out" | sed -n 's/^events ingested *\([0-9]*\).*/\1/p' | head -1)
  evs=$(printf '%s' "$out"     | sed -n 's/^events\/sec *\([0-9]*\).*/\1/p' | head -1)
  p50=$(printf '%s' "$out"     | sed -n 's/^latency p50\/p99 *\([0-9.]*\)ms.*/\1/p' | head -1)
  p99=$(printf '%s' "$out"     | sed -n 's/^latency p50\/p99 *[0-9.]*ms \/ \([0-9.]*\)ms.*/\1/p' | head -1)
  dropped=$(printf '%s' "$out" | sed -n 's/^dropped *\([0-9]*\).*/\1/p' | head -1)
  printf '%s %s %s %s %s %s %s' "${claimed:-0}" "$((after - before))" "${evs:-0}" "${p50:-0}" "${p99:-0}" "${dropped:-0}" "$rc"
}

median() { printf '%s\n' "$@" | sort -n | awk '{a[NR]=$1} END{print (NR%2==1)? a[(NR+1)/2] : a[NR/2]}'; }

say "protocol: warmup $WARMUP (discarded), $REPEATS repeats x $DURATION per config"
info "configs: $CONFIGS"
info "results: $RESULTS"

FAILURES=0
SUMMARY="$RESULTS/summary.txt"
: > "$SUMMARY"

for cfg in $CONFIGS; do
  label=$(printf '%s' "$cfg" | cut -d: -f1)
  rate=$(printf '%s' "$cfg" | cut -d: -f2)
  batch=$(printf '%s' "$cfg" | cut -d: -f3)

  say "$label — $rate ev/s, batch $batch"

  # Warmup, explicitly discarded. Its numbers are never reported.
  info "warmup ($WARMUP, discarded)"
  one_run "$rate" "$batch" "$WARMUP" "$label-warmup" >/dev/null

  evs_list=""; recon_ok=1
  for r in $(seq 1 "$REPEATS"); do
    set -- $(one_run "$rate" "$batch" "$DURATION" "$label-run$r")
    claimed=$1; written=$2; evs=$3; p50=$4; p99=$5; dropped=$6
    if [ "$claimed" = "$written" ]; then recon="OK"; else recon="** MISMATCH **"; recon_ok=0; fi
    printf '   run %s: %s ev/s  p50 %sms  p99 %sms  dropped %s  | claimed %s / written %s  %s\n' \
      "$r" "$evs" "$p50" "$p99" "$dropped" "$claimed" "$written" "$recon"
    evs_list="$evs_list $evs"
  done

  # shellcheck disable=SC2086
  med=$(median $evs_list)
  lo=$(printf '%s\n' $evs_list | sort -n | head -1)
  hi=$(printf '%s\n' $evs_list | sort -n | tail -1)
  spread="n/a"
  [ "${med:-0}" -gt 0 ] && spread=$(awk -v l="$lo" -v h="$hi" -v m="$med" 'BEGIN{printf "%.1f%%", (h-l)/m*100}')
  printf '%-10s median %s ev/s  (min %s, max %s, spread %s over %s runs)  reconciliation %s\n' \
    "$label" "$med" "$lo" "$hi" "$spread" "$REPEATS" \
    "$([ "$recon_ok" = "1" ] && echo PASS || echo FAIL)" | tee -a "$SUMMARY"
  [ "$recon_ok" = "1" ] || FAILURES=$((FAILURES + 1))
done

# Pull the k6 JSON off the load generator before anything destroys it.
if [ "$MODE" = "aws" ]; then
  say "collecting artifacts from the load generator"
  gen_sh 'cat summary.json 2>/dev/null' > "$RESULTS/k6-summary.json" 2>/dev/null || true
  [ -s "$RESULTS/k6-summary.json" ] && info "k6-summary.json retrieved" || info "no summary.json on the loadgen (non-fatal)"
fi

say "RESULTS"
cat "$SUMMARY"
info ""
info "artifacts: $RESULTS"
if [ "$FAILURES" -gt 0 ]; then
  info ""
  info "$FAILURES config(s) FAILED reconciliation: the database does not hold what"
  info "the load generator claims it sent. These numbers must NOT be published."
  exit 1
fi
info "every run reconciled: rows written == events claimed"
