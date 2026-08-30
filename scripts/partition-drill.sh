#!/usr/bin/env bash
# Severs a real network link between a leader-lease holder and Postgres, and
# measures how long the role stays held (ADR-114).
#
# Before ADR-114 leadership was a session advisory lock, and this drill measured
# how long a partitioned holder's lock stayed stranded — a number that depended
# on the NETWORK honouring TCP keepalives (95 s measured; 7875 s by default).
# The lease has no such dependency: the row expires on the database clock
# LeaseTTL (10 s) after the holder's last acknowledged renew, whatever the
# holder's socket is doing. This drill exists so that claim stays measured,
# not reasoned about.
#
#   ./scripts/partition-drill.sh          # expect RELEASED within ~10-13 s
#
# Requires the bench Postgres container with migrations applied (see
# docs/benchmarks/sustained-throughput.md). Uses the webhook_delivery role;
# never run against a database an app replica is using.
set -euo pipefail

PG_CONTAINER="${PG_CONTAINER:-velox-bench-pg}"
PG_PORT="${PG_PORT:-55432}"
PG_DB="${PG_DB:-velox}"
NET="velox-partition-drill"
HOLDER="velox-partition-holder"
ROLE="${ROLE:-webhook_delivery}"
TTL_S=10        # leader.LeaseTTL
BEAT_S=3        # leader.HeartbeatEvery
GIVE_UP="${GIVE_UP:-120}"

cleanup() {
  docker rm -f "$HOLDER" >/dev/null 2>&1 || true
  docker network disconnect "$NET" "$PG_CONTAINER" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  q "UPDATE leader_leases SET holder_id = NULL, acquired_at = NULL, heartbeat_at = NULL, expires_at = NULL WHERE role = '${ROLE}' AND holder_id = 'drill'" >/dev/null 2>&1 || true
}
q() { PGPASSWORD=velox psql -h localhost -p "$PG_PORT" -U velox -d "$PG_DB" -A -t -c "$1" 2>/dev/null; }
trap cleanup EXIT

docker network create "$NET" >/dev/null 2>&1 || true
docker network connect "$NET" "$PG_CONTAINER" >/dev/null 2>&1 || true

# The holder takes the role exactly the way Manager.acquire does (free or
# expired row → mine, token bumped) and then renews every BEAT_S seconds the
# way the heartbeat does. Each renew is its own statement on its own
# connection, so a severed link makes the NEXT renew hang — and the row
# expires TTL_S after the last one that landed. Nothing here depends on the
# holder noticing.
ACQ="UPDATE leader_leases SET holder_id='drill', holder_token=holder_token+1, acquired_at=now(), heartbeat_at=now(), expires_at=now()+interval '${TTL_S} seconds' WHERE role='${ROLE}' AND (expires_at IS NULL OR expires_at < now()) AND paused_at IS NULL RETURNING holder_token"
BEAT="UPDATE leader_leases SET heartbeat_at=now(), expires_at=now()+interval '${TTL_S} seconds' WHERE role='${ROLE}' AND holder_id='drill'"
docker run -d --name "$HOLDER" --network "$NET" postgres:16-alpine \
  sh -c "U='postgres://velox:velox@${PG_CONTAINER}:5432/${PG_DB}'; psql \"\$U\" -A -t -c \"${ACQ}\" || exit 1; while :; do sleep ${BEAT_S}; psql \"\$U\" -A -t -c \"${BEAT}\" >/dev/null || echo 'renew failed'; done" >/dev/null

for _ in $(seq 1 30); do
  held=$(q "SELECT held FROM leader_status WHERE role='${ROLE}' AND holder_id='drill'")
  [ "$held" = "t" ] && break
  sleep 1
done
[ "${held:-}" = "t" ] || { echo "FAIL: holder never acquired the ${ROLE} lease (is the role free and unpaused?)"; exit 1; }
# Let at least one renew land so the measurement starts from a renewed lease.
sleep $(( BEAT_S + 1 ))
echo "holder ready: role=${ROLE} held, renewing every ${BEAT_S}s, TTL ${TTL_S}s"

start=$(date +%s)
docker network disconnect "$NET" "$HOLDER"
echo "link severed (interface removed, no FIN sent) — predicted release within ${TTL_S}s of the last renew"

while :; do
  elapsed=$(( $(date +%s) - start ))
  if [ "$(q "SELECT held FROM leader_status WHERE role='${ROLE}'")" = "f" ]; then
    echo "RELEASED after ${elapsed}s (lease expired on the database clock)"
    tok=$(q "UPDATE leader_leases SET holder_id='drill-successor', holder_token=holder_token+1, acquired_at=now(), heartbeat_at=now(), expires_at=now()+interval '1 second' WHERE role='${ROLE}' AND expires_at < now() RETURNING holder_token" | head -1)
    [ -n "$tok" ] && echo "successor acquired the role (token ${tok}) — takeover confirmed" || echo "FAIL: successor could not acquire"
    [ "$(docker inspect -f '{{.State.Running}}' "$HOLDER" 2>/dev/null)" = "true" ] \
      && echo "holder process still alive — this measured a partition, not a process death"
    exit 0
  fi
  if [ "$elapsed" -ge "$GIVE_UP" ]; then
    echo "STILL HELD at ${elapsed}s — the lease did not expire; that is a bug, not a tuning question"
    exit 1
  fi
  sleep 1
done
