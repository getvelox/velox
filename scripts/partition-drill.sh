#!/usr/bin/env bash
# Severs a real network link between an advisory-lock holder and Postgres, and
# measures how long the lock stays stranded.
#
# This exists because the dead-leader recovery window was arithmetic for a
# while — we set tcp_keepalives on the lock connection and computed
# idle + interval*count. That is the one number in the failure benchmark that
# depends on the NETWORK honouring a setting, so it is the one worth severing a
# link to check rather than reasoning about.
#
#   ./scripts/partition-drill.sh 60 10 3     # production setting, expect ~90s
#   ./scripts/partition-drill.sh             # no SET: inherits the 7875s default
#
# Requires the bench Postgres container (see docs/benchmarks/sustained-throughput.md).
set -euo pipefail

PG_CONTAINER="${PG_CONTAINER:-velox-bench-pg}"
PG_PORT="${PG_PORT:-55432}"
NET="velox-partition-drill"
HOLDER="velox-partition-holder"
KEY="${KEY:-99888299}"
GIVE_UP="${GIVE_UP:-300}"

# No arguments means "inherit the server default", which is what the code did
# before the keepalive fix — the negative control.
if [ $# -eq 3 ]; then
  SETS="SET tcp_keepalives_idle=$1; SET tcp_keepalives_interval=$2; SET tcp_keepalives_count=$3;"
  PREDICTED=$(( $1 + $2 * $3 ))
  echo "keepalives ${1}/${2}/${3} -> predicted ${PREDICTED}s"
else
  SETS=""
  echo "no keepalive SET — inheriting the server default (predicted 7875s)"
fi

cleanup() {
  docker rm -f "$HOLDER" >/dev/null 2>&1 || true
  docker network disconnect "$NET" "$PG_CONTAINER" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker network create "$NET" >/dev/null 2>&1 || true
docker network connect "$NET" "$PG_CONTAINER" >/dev/null 2>&1 || true

# The holder MUST end up idle, not running a query. A backend executing
# pg_sleep never touches its socket, so it cannot notice the peer is gone no
# matter what the kernel decides — an earlier version of this drill did exactly
# that and reported a false negative. Piping the statements and then holding
# stdin open leaves psql connected and waiting for input: state='idle', which
# is how the scheduler actually holds this lock.
docker run -d --name "$HOLDER" --network "$NET" postgres:16-alpine \
  sh -c "(echo \"${SETS} SELECT pg_advisory_lock(${KEY});\"; sleep 3600) | psql 'postgres://velox:velox@${PG_CONTAINER}:5432/velox'" >/dev/null

q() { PGPASSWORD=velox psql -h localhost -p "$PG_PORT" -U velox -d velox -A -t -c "$1" 2>/dev/null; }

for _ in $(seq 1 30); do
  state=$(q "SELECT state FROM pg_stat_activity WHERE pid IN (SELECT pid FROM pg_locks WHERE locktype='advisory' AND objid=${KEY})")
  [ -n "$state" ] && break
  sleep 1
done
[ -n "${state:-}" ] || { echo "FAIL: holder never acquired the lock"; exit 1; }
[ "$state" = "idle" ] || { echo "FAIL: holder session is '$state', must be 'idle' — see comment above"; exit 1; }
echo "holder ready, session state=idle"

start=$(date +%s)
docker network disconnect "$NET" "$HOLDER"
echo "link severed (interface removed, no FIN sent)"

while :; do
  elapsed=$(( $(date +%s) - start ))
  if [ "$(q "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=${KEY}")" = "0" ]; then
    echo "RELEASED after ${elapsed}s"
    [ "$(docker inspect -f '{{.State.Running}}' "$HOLDER" 2>/dev/null)" = "true" ] \
      && echo "holder process still alive — this measured a partition, not a process death"
    exit 0
  fi
  if [ "$elapsed" -ge "$GIVE_UP" ]; then
    echo "STILL HELD at ${elapsed}s (gave up; raise GIVE_UP to wait longer)"
    exit 0
  fi
  sleep 2
done
