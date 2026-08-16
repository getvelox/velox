#!/usr/bin/env bash
# The whole benchmark, one command, in the order that works — and it stops at
# the first step that fails rather than measuring a broken rig.
#
#   ./run.sh                    # full run on AWS: provision -> measure -> teardown
#   KEEP=1 ./run.sh             # leave the rig up afterwards (it keeps billing)
#   TARGET=local ADMIN_DSN=postgres://... ./run.sh    # the same sequence, locally
#
# Every step is one of the scripts next to this file; this only sequences them,
# fails fast, and writes one log. Steps and what stops the run:
#
#   0  calibrate.sh          instrument must print CALIBRATED
#   0  teardown.sh --check   account must be CLEAN (exit 0); UNKNOWN stops here
#   1  provision.sh --yes    + watchdog armed (aws only)
#   2  bringup.sh            running, seeded, verified velox — or FATAL
#   3  seed-history.sh       SEED_ROWS rows of history (0 to skip)
#   4  measure.sh            SUSTAINED protocol, then the closed-loop ceiling
#   5  db-ceiling.sh         the denominator, fed the ev/s measure.sh reported
#   6  teardown.sh           unless KEEP=1; must end CLEAN
#
# The log is written to $OUT/run-<timestamp>.log as it goes, so it can be read
# from another terminal (or by whoever is helping) while the run is in flight.
set -uo pipefail

TARGET="${TARGET:-aws}"
OUT="${OUT:-$HOME/.velox-bench-rig}"
KEEP="${KEEP:-0}"
SEED_ROWS="${SEED_ROWS:-20000000}"
CONFIGS="${CONFIGS:-single:200:1 batched:1000:10}"
PROBE_RATE="${PROBE_RATE:-5}"
CEILING_BATCH="${CEILING_BATCH:-500}"
CEILING_VUS="${CEILING_VUS:-16}"
HERE="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$OUT"
LOG="$OUT/run-$(date -u +%Y%m%dT%H%M%SZ).log"
exec > >(tee -a "$LOG") 2>&1

step() { printf '\n\n########## [%s] STEP %s ##########\n' "$(date -u +%FT%TZ)" "$*"; }
fail() { printf '\n########## RUN STOPPED at: %s ##########\nlog: %s\n' "$*" "$LOG"; [ "$TARGET" = "aws" ] && printf 'the rig may still be up — ./teardown.sh --check, then ./teardown.sh\n'; exit 1; }

export TARGET OUT
printf 'velox benchmark run — target=%s  log=%s\n' "$TARGET" "$LOG"

step "0a calibrate the instrument"
"$HERE/calibrate/calibrate.sh" || fail "calibration (the measuring tool did not report the truth)"

if [ "$TARGET" = "aws" ]; then
  step "0b confirm the account is clean"
  "$HERE/teardown.sh" --check; rc=$?
  [ "$rc" = "0" ] || fail "teardown --check exit $rc (1 = something is billing already, 2 = could not look)"

  step "1 provision (billing starts) + arm the watchdog"
  "$HERE/provision.sh" --yes || fail "provision"
  ( nohup "$HERE/watchdog.sh" 240 >"$OUT/watchdog.log" 2>&1 & )
  printf 'watchdog armed (240 min) -> %s\n' "$OUT/watchdog.log"
fi

step "2 bring-up (running, seeded, verified)"
if [ "$TARGET" = "aws" ]; then
  MODE=aws "$HERE/bringup.sh" || fail "bringup"
else
  MODE=local "$HERE/bringup.sh" || fail "bringup"
fi
CREDS="$OUT/bench-creds.json"

if [ "${SEED_ROWS:-0}" -gt 0 ]; then
  step "3 seed $SEED_ROWS rows of history"
  if [ "$TARGET" = "aws" ]; then "$HERE/seed-history.sh" "$SEED_ROWS" || fail "seed-history"
  else DATABASE_URL="$ADMIN_DSN" TARGET=local "$HERE/seed-history.sh" "$SEED_ROWS" || fail "seed-history"; fi

  # SETTLE after a bulk load before measuring. On the first real run, the
  # first 10-minute repeat started ~60 s after a 22M-row seed finished and hit
  # a single 5-second stall 20 s in — 499 slow requests, 298 dropped
  # iterations, the repeat correctly FAILED — while RDS write IOPS ran at 2x
  # baseline for the first five minutes: autovacuum and checkpointing working
  # through the load. The 30 s warmup cannot absorb that; a settle can. Nothing
  # else in the following 9.5 minutes, nor in the next repeat, stalled.
  SETTLE="${SETTLE:-$([ "$TARGET" = "aws" ] && echo 600 || echo 0)}"
  if [ "$SETTLE" -gt 0 ]; then
    step "3b settle ${SETTLE}s after the bulk load (autovacuum / checkpoint) before measuring"
    sleep "$SETTLE"
  fi
fi

step "4a measure — SUSTAINED protocol: $CONFIGS (probe $PROBE_RATE/s)"
SUSTAINED="${SUSTAINED:-1}" CONFIGS="$CONFIGS" PROBE_RATE="$PROBE_RATE" CREDS="$CREDS" "$HERE/measure.sh"
rc=$?; SUMMARY_DIR=$(ls -dt "$OUT"/results-* | head -1)
[ "$rc" = "0" ] || printf '\nNOTE: measure.sh exit %s — at least one repeat FAILED its gate; see [FAIL:...] lines above. Continuing to the ceiling and denominator so the run is complete, but do not publish a config with a failed repeat.\n' "$rc"

step "4b measure — closed-loop ceiling: $CEILING_VUS VUs, batch $CEILING_BATCH, 90s"
K6_MODE=max VUS="$CEILING_VUS" CONFIGS="ceiling:0:$CEILING_BATCH" DURATION="${CEIL_DURATION:-90s}" REPEATS=1 WARMUP="${CEIL_WARMUP:-20s}" SUSTAINED=0 PROBE_RATE=0 CREDS="$CREDS" "$HERE/measure.sh" || printf '\nNOTE: ceiling run failed its gate (see above)\n'

step "5 denominator — pgbench on the same DB, batch $CEILING_BATCH (compared against the closed-loop ceiling)"
# Compare ceilings to ceilings: the numerator is the CLOSED-LOOP ev/s Velox
# reached (a maximum), against the DB's own maximum. Feeding the rate-controlled
# offered rate here would just restate the rate we chose.
CEIL_DIR=$(ls -dt "$OUT"/results-* | head -1)
EVS=$(grep -E '^ceiling ' "$CEIL_DIR/summary.txt" 2>/dev/null | grep -oE 'ceiling median [0-9]+' | grep -oE '[0-9]+' | head -1)
[ -n "$EVS" ] || EVS=$(grep -E '^batched ' "$SUMMARY_DIR/summary.txt" 2>/dev/null | grep -oE 'offered [0-9]+' | grep -oE '[0-9]+' | head -1)
if [ "$TARGET" = "aws" ]; then VELOX_EVS="${EVS:-}" BATCH="$CEILING_BATCH" "$HERE/db-ceiling.sh" || printf '\nNOTE: db-ceiling failed (non-fatal for the run)\n'
else DATABASE_URL="$ADMIN_DSN" TARGET=local VELOX_EVS="${EVS:-}" BATCH="$CEILING_BATCH" DURATION="${PGB_DURATION:-30}" "$HERE/db-ceiling.sh" || printf '\nNOTE: db-ceiling failed (non-fatal)\n'; fi

if [ "$TARGET" = "aws" ]; then
  if [ "$KEEP" = "1" ]; then
    step "6 KEEP=1 — rig left UP and BILLING; tear down with ./teardown.sh"
  else
    step "6 teardown"
    "$HERE/teardown.sh"; rc=$?
    [ "$rc" = "0" ] || fail "teardown did not reach CLEAN (exit $rc) — resources may still be billing"
  fi
fi

printf '\n########## RUN COMPLETE ##########\nresults: %s\nlog:     %s\n' "$(ls -dt "$OUT"/results-* | head -1)" "$LOG"
cat "$OUT"/results-*/summary.txt 2>/dev/null | tail -6
