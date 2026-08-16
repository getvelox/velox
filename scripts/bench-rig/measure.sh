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
# A run PASSES only if ALL of these hold — and a run that fails any of them is
# excluded from every statistic and fails the whole invocation:
#     k6 exit 0          (every threshold held: errors, ingest p99, probe p99)
#     dropped == 0       (the offered rate was actually offered)
#     requests failed==0
#     claimed  >  0      (a dead server reconciles 0 == 0; that is not a pass)
#     claimed == written (the database holds what the client says it sent)
#
# The first version gated on the last line only, so a run in which every
# request failed, or the rate was never sustained, or k6 itself said the
# thresholds were breached, still printed "reconciliation PASS" and entered
# the median. Reconciliation is blind to server errors by construction — only
# 2xx are claimed — so it can never be the only gate.
#
# The published statistic is LATENCY (p50/p99 across passing repeats), not
# events/sec: in rate mode delivered ev/s equals the offered RATE whenever
# drops are zero, so a "median ev/s with 0.2% spread" is the generator keeping
# its own schedule, not anything about Velox.
#
#   TARGET=local ADMIN_DSN=postgres://... ./measure.sh
#   TARGET=aws   ./measure.sh
#
# CONFIGS is a space-separated list of label:RATE:BATCH triples.
# K6_MODE=max runs the closed-loop ceiling instead (VUS sets the load there).
set -uo pipefail

TARGET="${TARGET:-aws}"
K6_MODE="${K6_MODE:-rate}"
VUS="${VUS:-}"
P99_MS="${P99_MS:-0}"
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
API_KEY=$(jqf api_key)
CUSTOMERS=$(jqf customer_count); [ -n "$CUSTOMERS" ] || CUSTOMERS=1
CUSTOMER_PREFIX=$(jqf external_customer_id_prefix)
CUSTOMER_ID_PREFIX=$(jqf customer_id_prefix)

HERE="$(cd "$(dirname "$0")" && pwd)"

if [ "$TARGET" = "aws" ]; then
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
  [ -n "${ADMIN_DSN:-}" ] || die "TARGET=local requires ADMIN_DSN"
  app_sh() { bash -c "$*"; }
  gen_sh() { bash -c "$*"; }
  # bringup.sh records the base URL it verified into the creds file; measure
  # must not guess a different port (the first version defaulted to :8099 while
  # bringup served :8080, and a dead port reconciled 0 == 0 as PASS).
  BASE="${BASE:-$(jqf base_url)}"
  [ -n "$BASE" ] || die "no BASE and no base_url in $CREDS — run ./bringup.sh first"
  SCRIPT_PATH="${SCRIPT_PATH:-$HERE/ingest.js}"
fi

rows() { app_sh "psql '$ADMIN_DSN' -qtA -c \"SELECT count(*) FROM usage_events WHERE tenant_id='vlx_ten_bench'\"" | tr -d '[:space:]'; }
# The table a run writes into is part of the measurement — index maintenance
# grows with volume, and an empty table is the optimistic case. Record it.
table_state() { app_sh "psql '$ADMIN_DSN' -qtA -c \"SELECT count(*)||' rows, '||pg_size_pretty(pg_table_size('usage_events'))||' heap, '||pg_size_pretty(pg_indexes_size('usage_events'))||' idx' FROM usage_events\"" | tr -d '\n'; }

# One k6 invocation. Echoes: events_claimed rows_written ev_per_sec p50 p99 dropped
one_run() {
  local rate=$1 batch=$2 dur=$3 tag=$4
  local before after out claimed evs p50 p99 dropped
  before=$(rows)
  gen_sh "k6 run --quiet --summary-export /tmp/k6-$tag.json \
    -e BASE=$BASE -e API_KEY=$API_KEY -e CUSTOMERS=$CUSTOMERS \
    -e CUSTOMER_PREFIX=$CUSTOMER_PREFIX -e CUSTOMER_ID_PREFIX=$CUSTOMER_ID_PREFIX \
    -e RATE=$rate -e BATCH=$batch -e DURATION=$dur -e MODE=$K6_MODE ${VUS:+-e VUS=$VUS} \
    -e P99_MS=$P99_MS -e PROBE_RATE=$PROBE_RATE -e PROBE_P99_MS=$PROBE_P99_MS \
    $SCRIPT_PATH" > "$RESULTS/$tag.txt" 2>&1
  local rc=$?
  after=$(rows)
  gen_sh "cat /tmp/k6-$tag.json" > "$RESULTS/$tag.k6.json" 2>/dev/null || true
  out=$(cat "$RESULTS/$tag.txt")
  claimed=$(printf '%s' "$out" | sed -n 's/^events ingested *\([0-9]*\).*/\1/p' | head -1)
  evs=$(printf '%s' "$out"     | sed -n 's/^events\/sec *\([0-9]*\).*/\1/p' | head -1)
  p50=$(printf '%s' "$out"     | sed -n 's/^latency p50\/p99 *\([0-9.]*\)ms.*/\1/p' | head -1)
  p99=$(printf '%s' "$out"     | sed -n 's/^latency p50\/p99 *[0-9.]*ms \/ \([0-9.]*\)ms.*/\1/p' | head -1)
  dropped=$(printf '%s' "$out" | sed -n 's/^dropped *\([0-9]*\).*/\1/p' | head -1)
  failed=$(printf '%s' "$out"  | sed -n 's/^requests failed *\([0-9]*\).*/\1/p' | head -1)
  samples=$(printf '%s' "$out" | sed -n 's/^latency samples *\([0-9]*\).*/\1/p' | head -1)
  probe="n/a"
  if [ "$PROBE_RATE" -gt 0 ]; then
    printf '%s' "$out" | grep -q "RESPONSIVE under load" && probe="RESPONSIVE"
    printf '%s' "$out" | grep -q "DEGRADED" && probe="DEGRADED"
  fi
  printf '%s %s %s %s %s %s %s %s %s %s' "${claimed:-0}" "$((after - before))" "${evs:-0}" "${p50:-0}" "${p99:-0}" "${dropped:-0}" "$rc" "${failed:-0}" "${samples:-0}" "$probe"
}

median() { printf '%s\n' "$@" | sort -n | awk '{a[NR]=$1} END{ if (NR==0) print "n/a"; else if (NR%2==1) print a[(NR+1)/2]; else printf "%.1f\n", (a[NR/2]+a[NR/2+1])/2 }'; }
minof()  { printf '%s\n' "$@" | sort -n | head -1; }
maxof()  { printf '%s\n' "$@" | sort -n | tail -1; }

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

  p50s=""; p99s=""; passed=0; failed_runs=0
  for r in $(seq 1 "$REPEATS"); do
    state=$(table_state)
    set -- $(one_run "$rate" "$batch" "$DURATION" "$label-run$r")
    claimed=$1; written=$2; evs=$3; p50=$4; p99=$5; dropped=$6; rc=$7; nfail=$8; samples=$9; probe=${10}

    # THE GATE. Every clause is a way a bad run used to enter the median.
    reasons=""
    [ "$rc" = "0" ]              || reasons="$reasons k6-exit=$rc"
    [ "${dropped:-0}" = "0" ]    || reasons="$reasons dropped=$dropped"
    [ "${nfail:-0}" = "0" ]      || reasons="$reasons failed=$nfail"
    [ "${claimed:-0}" -gt 0 ]    || reasons="$reasons claimed=0"
    [ "$claimed" = "$written" ]  || reasons="$reasons claimed!=written($claimed/$written)"
    [ "$probe" != "DEGRADED" ]   || reasons="$reasons probe=DEGRADED"

    if [ -z "$reasons" ]; then
      verdict="PASS"; passed=$((passed+1)); p50s="$p50s $p50"; p99s="$p99s $p99"
    else
      verdict="FAIL:$reasons"; failed_runs=$((failed_runs+1))
    fi
    printf '   run %s [%s]: %s ev/s  p50 %sms  p99 %sms (n=%s)  dropped %s  failed %s  probe %s | claimed %s / written %s  %s\n' \
      "$r" "$state" "$evs" "$p50" "$p99" "$samples" "$dropped" "$nfail" "$probe" "$claimed" "$written" "$verdict" | tee -a "$RESULTS/runs.txt"
  done

  # Statistics over PASSING repeats only, on the variables that actually vary.
  # shellcheck disable=SC2086
  if [ "$passed" -gt 0 ]; then
    line=$(printf '%-10s offered %s ev/s batch %s | %s/%s passed | p50 median %sms (%s-%s) | p99 median %sms (%s-%s)' \
      "$label" "$rate" "$batch" "$passed" "$REPEATS" \
      "$(median $p50s)" "$(minof $p50s)" "$(maxof $p50s)" \
      "$(median $p99s)" "$(minof $p99s)" "$(maxof $p99s)")
  else
    line=$(printf '%-10s offered %s ev/s batch %s | 0/%s passed — NOT SUSTAINED at this rate' "$label" "$rate" "$batch" "$REPEATS")
  fi
  echo "$line" | tee -a "$SUMMARY"
  [ "$failed_runs" -eq 0 ] || FAILURES=$((FAILURES + 1))
done

# Pull the k6 JSON off the load generator before anything destroys it.
if [ "$TARGET" = "aws" ]; then
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
  info "$FAILURES config(s) had at least one FAILED run (see [FAIL:...] above)."
  info "A config is only 'sustained' at a rate when EVERY repeat passes. Do not publish"
  info "a config with a failed repeat; lower the rate or fix the cause and re-run."
  exit 1
fi
info "every repeat of every config passed: thresholds held, nothing dropped, nothing failed,"
info "and the database holds exactly what the client claims it sent."
