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
set -uo pipefail
MINUTES="${1:-240}"
HERE="$(cd "$(dirname "$0")" && pwd)"
DEADLINE=$(( $(date +%s) + MINUTES * 60 ))

echo "watchdog armed: force teardown at $(date -r "$DEADLINE" 2>/dev/null || date -d "@$DEADLINE") (${MINUTES}m)"
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  # Nothing left to guard — the run tore down cleanly, so stand down.
  if "$HERE/teardown.sh" --check 2>/dev/null | grep -q '^CLEAN'; then
    echo "$(date -u +%FT%TZ) rig already clean; watchdog standing down"
    exit 0
  fi
  sleep 60
done
echo "$(date -u +%FT%TZ) DEADLINE REACHED — forcing teardown"
"$HERE/teardown.sh"
