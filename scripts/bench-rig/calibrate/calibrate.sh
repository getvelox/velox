#!/usr/bin/env bash
# Calibrates the load generator against a system whose behaviour we control.
#
# ingest.js reports numbers about Velox, and nothing about Velox tells us
# whether those numbers are true. This points the SAME script at a stub with a
# known service time and an exact server-side counter, then checks that what
# the generator reports matches what actually happened.
#
# It exists because measurement bugs here are silent by nature — they produce
# confident numbers rather than errors. Every one found so far did:
#
#   - the pre-k6 generator fired all workers in lockstep from slot 0. The MEAN
#     rate was right, so it survived a full round of AWS measurement while its
#     self-inflicted queueing was charged to Velox (median 24.8ms -> 6.1ms once
#     fixed).
#   - ingest.js read values['p(99)'], which k6 does not compute by default. It
#     came back undefined and printed as a confident 0.0ms.
#   - ingest.js sized VUs for a 100ms tail, so at 100 ev/s against a 7.4ms p50
#     it reported dropped iterations — the GENERATOR failing to offer the rate,
#     which reads as Velox failing to sustain it.
#
# Three cases, and the two negative controls matter more than the positive one:
# an instrument that always reports success is not an instrument.
#
#   1. POSITIVE   healthy server -> exact event count, exact rate, p50 == truth
#   2. NEGATIVE   server erroring -> failures reported, NOT counted as delivered
#   3. NEGATIVE   server too slow -> rate reported honestly, drops flagged
#
# Usage: ./calibrate.sh          (needs k6 and go; nothing else, no AWS)
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
INGEST="$HERE/../ingest.js"
PORT="${PORT:-8123}"
BASE="http://localhost:$PORT"
STUB=$(mktemp -d)/stub
FAILURES=0

command -v k6 >/dev/null || { echo "FATAL: k6 not installed"; exit 1; }
[ -f "$INGEST" ] || { echo "FATAL: $INGEST not found"; exit 1; }

go build -o "$STUB" "$HERE/stub.go" || { echo "FATAL: stub build failed"; exit 1; }
trap 'pkill -f "$STUB" 2>/dev/null; rm -rf "$(dirname "$STUB")"' EXIT

start_stub() {
  pkill -f "$STUB" 2>/dev/null; sleep 0.3
  env "$@" "$STUB" >/dev/null 2>&1 &
  disown 2>/dev/null || true
  for _ in $(seq 1 40); do
    curl -sf "$BASE/count" >/dev/null 2>&1 && return 0
    sleep 0.25
  done
  echo "FATAL: stub never came up on $PORT"; exit 1
}
truth() { curl -s "$BASE/count"; }
field() { python3 -c "import sys,json;print(json.load(sys.stdin)['$1'])"; }

check() { # label actual expected tolerance
  local label="$1" actual="$2" expected="$3" tol="${4:-0}"
  local lo=$(python3 -c "print($expected-$tol)") hi=$(python3 -c "print($expected+$tol)")
  if python3 -c "import sys;sys.exit(0 if $lo <= $actual <= $hi else 1)"; then
    printf '    %-38s %-12s OK\n' "$label" "$actual"
  else
    printf '    %-38s %-12s ** WRONG ** (want %s +/-%s)\n' "$label" "$actual" "$expected" "$tol"
    FAILURES=$((FAILURES+1))
  fi
}

run_k6() { # rate batch duration [extra env args...]
  local rate=$1 batch=$2 dur=$3; shift 3
  k6 run --quiet -e BASE="$BASE" -e API_KEY=calibration -e RATE="$rate" \
    -e BATCH="$batch" -e DURATION="$dur" "$@" "$INGEST" 2>&1
}
num() { grep -E "^$1" /tmp/k6calib.txt | grep -oE '[0-9.]+' | head -1; }
# p50 needs its own parser: the summary line reads "latency p50/p99  20.3ms /
# 53.9ms", so a naive first-number grep returns the 50 out of "p50". It did,
# and the run correctly reported NOT CALIBRATED rather than passing.
p50() { sed -n 's/^latency p50\/p99 *\([0-9.]*\)ms.*/\1/p' /tmp/k6calib.txt | head -1; }

echo
echo "=== 1/3 POSITIVE — healthy server, 20ms known service time ==="
start_stub DELAY_MS=20
for spec in "200 1" "500 10"; do
  set -- $spec; rate=$1; batch=$2
  before=$(truth | field events)
  run_k6 "$rate" "$batch" 15s > /tmp/k6calib.txt
  after=$(truth | field events)
  echo "  offered $rate ev/s, batch $batch:"
  check "events match server truth" "$(num 'events ingested')" "$((after-before))" 0
  check "delivered rate == offered"  "$(num 'events/sec')"      "$rate"            "$(python3 -c "print(max(2,$rate*0.03))")"
  check "p50 recovers the 20ms truth" "$(p50)" 20 6
  check "no spurious drops"           "$(num 'dropped')"        0                  0
done

echo
echo "=== 2/3 NEGATIVE — server fails ~10% of requests ==="
echo "    a failed request must be reported, and must NOT count as throughput"
start_stub DELAY_MS=20 FAIL_PCT=10
before_ev=$(truth | field events)
run_k6 200 1 15s > /tmp/k6calib.txt
t=$(truth); after_ev=$(echo "$t" | field events); rejected=$(echo "$t" | field rejected)
check "ingested == server SUCCESSES only" "$(num 'events ingested')" "$((after_ev-before_ev))" 0
check "failures reported"                 "$(num 'requests failed')" "$rejected"               2
if grep -q "http_req_failed" /tmp/k6calib.txt; then
  printf '    %-38s %-12s OK\n' "error threshold breached" "yes"
else
  printf '    %-38s %-12s ** WRONG ** (a run with errors must fail)\n' "error threshold breached" "no"
  FAILURES=$((FAILURES+1))
fi

echo
echo "=== 3/3 NEGATIVE — server too slow to sustain the offered rate ==="
echo "    2s/request against 20 VUs is ~10 req/s of capacity vs 200 offered"
start_stub DELAY_MS=2000
before_ev=$(truth | field events)
run_k6 200 1 15s -e VUS=20 > /tmp/k6calib.txt
after_ev=$(truth | field events)
check "events match server truth"    "$(num 'events ingested')" "$((after_ev-before_ev))" 0
check "p50 recovers the 2000ms truth" "$(p50)" 2000 150
dropped=$(num 'dropped')
if [ "${dropped:-0}" -gt 0 ] && grep -q "RATE NOT SUSTAINED" /tmp/k6calib.txt; then
  printf '    %-38s %-12s OK\n' "unsustained rate flagged" "$dropped dropped"
else
  printf '    %-38s %-12s ** WRONG ** (silently claimed the rate)\n' "unsustained rate flagged" "${dropped:-0}"
  FAILURES=$((FAILURES+1))
fi
rate_reported=$(num 'events/sec')
if python3 -c "import sys;sys.exit(0 if $rate_reported < 190 else 1)"; then
  printf '    %-38s %-12s OK\n' "reported rate is the DELIVERED one" "$rate_reported ev/s"
else
  printf '    %-38s %-12s ** WRONG ** (reported the offered rate)\n' "reported rate is the DELIVERED one" "$rate_reported"
  FAILURES=$((FAILURES+1))
fi

echo
if [ "$FAILURES" -eq 0 ]; then
  echo "CALIBRATED — the load generator reported the truth in all three cases."
else
  echo "NOT CALIBRATED — $FAILURES check(s) wrong. Benchmark numbers from this"
  echo "generator cannot be trusted until these pass."
fi
exit $((FAILURES > 0))
