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
#   4. NEGATIVE   run it TWICE -> no idempotency key is ever replayed
#   5. POSITIVE   known bimodal tail -> p99 recovers the TAIL truth, p50 the base
#   6. NEGATIVE   known slope -> the drift line reports last-third >> first-third
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

# Kill whatever is LISTENING ON THE PORT, not just this run's binary: the stub
# lives at a fresh mktemp path every invocation, so a stub left behind by an
# earlier run (a calibrate killed mid-pipeline before its EXIT trap fired) kept
# port 8123, the new one failed to bind, and the OLD one — with a different
# configuration — answered every request. Case 2 then counted 525 failures
# against an expected 300, and a full run stopped at step 0 for it.
kill_port() {
  local pids
  pids=$(lsof -ti "tcp:$PORT" 2>/dev/null || fuser "$PORT/tcp" 2>/dev/null)
  [ -n "$pids" ] && { kill $pids 2>/dev/null; sleep 0.3; }
  return 0
}
start_stub() {
  kill_port
  pkill -f "$STUB" 2>/dev/null; sleep 0.2
  # Each stub answers /whoami with the id it was started with, so we can prove
  # the process answering is the one we just launched and not a stale one.
  local id; id="calib-$$-$RANDOM"
  env STUB_ID="$id" "$@" "$STUB" >/dev/null 2>&1 &
  disown 2>/dev/null || true
  for _ in $(seq 1 40); do
    if [ "$(curl -sf "$BASE/whoami" 2>/dev/null)" = "$id" ]; then return 0; fi
    sleep 0.25
  done
  echo "FATAL: the stub answering on $PORT is not the one just started (stale process on the port?)"; exit 1
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
  # k6 exposes the PROCESS environment in __ENV, so any knob ingest.js reads
  # (PROBE_RATE, VUS, MODE, P99_MS, CUSTOMERS, RUN_ID, ...) that happens to be
  # exported by the caller silently reconfigures the calibration. run.sh
  # exports PROBE_RATE=5 for the real measurement; here that turned the probe
  # scenario ON against the stub and case 2 counted 225 extra 404s. Scrub them.
  env -u PROBE_RATE -u PROBE_P99_MS -u VUS -u MODE -u P99_MS -u CUSTOMERS -u CUSTOMER_ID \
      -u CUSTOMER_PREFIX -u CUSTOMER_ID_PREFIX -u RUN_ID -u EVENT -u RATE -u BATCH -u DURATION \
    k6 run --quiet -e BASE="$BASE" -e API_KEY=calibration -e RATE="$rate" \
      -e BATCH="$batch" -e DURATION="$dur" -e PROBE_RATE=0 -e MODE=rate "$@" "$INGEST" 2>&1
}
num() { grep -E "^$1" /tmp/k6calib.txt | grep -oE '[0-9.]+' | head -1; }
# p50 needs its own parser: the summary line reads "latency p50/p99  20.3ms /
# 53.9ms", so a naive first-number grep returns the 50 out of "p50". It did,
# and the run correctly reported NOT CALIBRATED rather than passing.
p50() { sed -n 's/^latency p50\/p99 *\([0-9.]*\)ms.*/\1/p' /tmp/k6calib.txt | head -1; }
p99() { sed -n 's/^latency p50\/p99 *[0-9.]*ms \/ \([0-9.]*\)ms.*/\1/p' /tmp/k6calib.txt | head -1; }

echo
echo "=== 1/6 POSITIVE — healthy server, 20ms known service time ==="
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
echo "=== 2/6 NEGATIVE — server fails ~10% of requests ==="
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
echo "=== 3/6 NEGATIVE — server too slow to sustain the offered rate ==="
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
echo "=== 4/6 NEGATIVE — no idempotency key may repeat, ACROSS RUNS ==="
echo "    a replayed key is deduplicated by a real endpoint, so the run would"
echo "    measure the dedupe path and report it as ingest throughput"
start_stub DELAY_MS=1
run_k6 200 1 10s > /tmp/k6calib.txt
run_k6 200 10 10s > /tmp/k6calib.txt
dups=$(truth | field duplicates)
if [ "${dups:-0}" -eq 0 ]; then
  printf '    %-38s %-12s OK\n' "duplicate idempotency keys" "0"
else
  printf '    %-38s %-12s ** WRONG ** (runs replay keys; throughput is fiction)\n' "duplicate idempotency keys" "$dups"
  FAILURES=$((FAILURES+1))
fi

echo
echo "=== 5/6 POSITIVE — a KNOWN tail: 5% of requests take 200ms, the rest 20ms ==="
echo "    the p99 is the number that gets published, and until now only p50 had a truth"
start_stub DELAY_MS=20 TAIL_PCT=5 TAIL_MS=200
run_k6 200 1 15s > /tmp/k6calib.txt
check "p50 still recovers the 20ms base"  "$(p50)" 20 6
check "p99 recovers the 200ms tail truth" "$(p99)" 200 25
echo
echo "=== 6/6 NEGATIVE — a KNOWN slope: +20us per request served, so latency climbs all run ==="
echo "    a 90s run can hide the slope a 10-minute run reveals; the drift line must see it"
start_stub DELAY_MS=5 RAMP_US=20
run_k6 200 1 15s > /tmp/k6calib.txt
driftx=$(sed -n 's/^drift p99 first\/last .*(x\([0-9.]*\)).*/\1/p' /tmp/k6calib.txt | head -1)
if awk -v d="${driftx:-0}" 'BEGIN{exit !(d > 2.0)}'; then
  printf '    %-38s %-12s OK\n' "drift factor reports the slope (>2x)" "x$driftx"
else
  printf '    %-38s %-12s ** WRONG ** (a steady slope went unreported)\n' "drift factor reports the slope (>2x)" "x${driftx:-0}"
  FAILURES=$((FAILURES+1))
fi
echo
if [ "$FAILURES" -eq 0 ]; then
  echo "CALIBRATED — the load generator reported the truth in all six cases."
else
  echo "NOT CALIBRATED — $FAILURES check(s) wrong. Benchmark numbers from this"
  echo "generator cannot be trusted until these pass."
fi
exit $((FAILURES > 0))
