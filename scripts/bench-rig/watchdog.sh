#!/usr/bin/env bash
# Force-tears-down the rig after a hard deadline, independent of whoever
# started it.
#
# This exists for one failure mode that teardown.sh cannot cover: teardown only
# runs if something is alive to run it. If the session driving the benchmark
# dies — crash, closed terminal, killed process — the rig keeps billing until a
# human notices. Detaching this watchdog means the deadline survives the thing
# that set it.
#
#   ( nohup ./watchdog.sh 240 >/tmp/velox-rig-watchdog.log 2>&1 & )
#
# It is a backstop, not the normal path: the benchmark runner tears down when
# it finishes, and this only fires if that never happened.
#
# THREE RULES, each of which replaces a way the earlier version could stand
# down while the account was still billing:
#
#   1. Trust teardown.sh's EXIT CODE, not its output text. It used to grep for
#      "^CLEAN", and the old teardown printed exactly that when its AWS calls
#      FAILED — so an expired credential silently disarmed the guard. Only
#      exit 0 means clean; exit 2 means "could not look", which is not clean.
#   2. Require the rig to have been SEEN ALIVE before a clean reading can
#      disarm anything, and require consecutive clean readings. Otherwise a
#      watchdog armed a moment before the instances appear reads "clean" once
#      and exits forever, guarding nothing.
#   3. VERIFY the deadline teardown actually worked, and retry. Security groups
#      cannot be deleted while an ENI still references them, so the first
#      attempt legitimately fails and the old version never noticed.
set -uo pipefail
MINUTES="${1:-240}"
HERE="$(cd "$(dirname "$0")" && pwd)"
DEADLINE=$(( $(date +%s) + MINUTES * 60 ))
CLEAN_STREAK_REQUIRED="${CLEAN_STREAK_REQUIRED:-2}"
# Configurable so the guard logic can actually be tested; a 60s poll makes
# every scenario a minutes-long experiment, which is how it went untested.
POLL_SECONDS="${POLL_SECONDS:-60}"

log() { echo "$(date -u +%FT%TZ) $*"; }

log "watchdog armed: force teardown at $(date -r "$DEADLINE" 2>/dev/null || date -d "@$DEADLINE") (${MINUTES}m)"

seen_alive=0
clean_streak=0
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  "$HERE/teardown.sh" --check >/dev/null 2>&1
  # Capture immediately: $? is clobbered by the very next command, and the
  # first version logged the exit status of an ASSIGNMENT (always 0) while
  # claiming it was teardown's.
  rc=$?
  case "$rc" in
    0)  # clean
        clean_streak=$(( clean_streak + 1 ))
        if [ "$seen_alive" = "1" ] && [ "$clean_streak" -ge "$CLEAN_STREAK_REQUIRED" ]; then
          log "rig torn down cleanly ($clean_streak consecutive clean checks); watchdog standing down"
          exit 0
        fi
        ;;
    1)  # not clean — the rig is up, which is what we are here to guard
        seen_alive=1
        clean_streak=0
        ;;
    *)  # UNKNOWN: the check could not see the account. NEVER treat as clean.
        clean_streak=0
        log "WARNING: teardown.sh --check could not determine state (exit $rc). Still guarding."
        ;;
  esac
  sleep "$POLL_SECONDS"
done

log "DEADLINE REACHED — forcing teardown"
for attempt in 1 2 3 4 5; do
  "$HERE/teardown.sh" >/dev/null 2>&1
  "$HERE/teardown.sh" --check >/dev/null 2>&1
  rc=$?
  if [ "$rc" = "0" ]; then
    log "teardown verified clean after attempt $attempt"
    exit 0
  fi
  log "teardown attempt $attempt did not reach clean (check exit $rc); retrying"
  sleep "$POLL_SECONDS"
done

# Do not exit 0 here. Something is still billing and a human must look.
log "ALARM: rig STILL NOT CLEAN after 5 teardown attempts — resources are billing."
log "ALARM: run scripts/bench-rig/teardown.sh by hand and check the AWS console."
exit 1
