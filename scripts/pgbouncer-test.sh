#!/usr/bin/env bash
# Runs the integration suite with the APP pool behind PgBouncer in transaction
# mode (ADR-114 PR-E). The admin pool (migrations, truncation, row forcing)
# stays direct, as production migrations do.
#
#   scripts/pgbouncer-test.sh                 # local: docker Postgres on :5432
#   PGB_MODE=ci scripts/pgbouncer-test.sh     # CI: --network host, DB at 127.0.0.1
#   PGB_PACKAGES="./internal/platform/leader/" scripts/pgbouncer-test.sh
#
# Any statement that assumes a session — SET without LOCAL, a session
# advisory lock, a prepared statement across transactions — fails here and
# passes on a direct connection. That difference is the whole point.
set -euo pipefail

MODE="${PGB_MODE:-local}"
IMAGE="${PGB_IMAGE:-edoburu/pgbouncer:v1.24.1-p1}"
NAME="velox-pgbouncer-test"
PKGS="${PGB_PACKAGES:-./...}"
DIR="$(cd "$(dirname "$0")" && pwd)/pgbouncer"
CONF="$(mktemp -d)"

if [ "$MODE" = "ci" ]; then
  DB_HOST=127.0.0.1; NETFLAGS=(--network host)
else
  # PgBouncer resolves hosts with its own resolver (evdns), which does not
  # see Docker's magic host.docker.internal name — hand it the IP.
  DB_HOST=$(docker run --rm --add-host=host.docker.internal:host-gateway alpine:3 getent ahostsv4 host.docker.internal | awk '{print $1}' | head -1)
  [ -n "$DB_HOST" ] || { echo "could not resolve the Docker host IP"; exit 1; }
  NETFLAGS=(-p 6432:6432 --add-host=host.docker.internal:host-gateway)
fi
sed "s/@DB_HOST@/${DB_HOST}/g" "$DIR/pgbouncer.ini.tmpl" > "$CONF/pgbouncer.ini"
cp "$DIR/userlist.txt" "$CONF/userlist.txt"
# The image's entrypoint runs as an unprivileged user and touches the
# userlist; a mktemp dir is mode 700 and root-owned on a Linux runner, so
# open it up (test-only, throwaway) and mount it read-write.
chmod 777 "$CONF" && chmod 666 "$CONF"/*

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; rm -rf "$CONF"; }
trap cleanup EXIT
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" "${NETFLAGS[@]}" -v "$CONF:/etc/pgbouncer" "$IMAGE" >/dev/null

# Each probe is bounded (perl alarm — portable): a client of a PgBouncer
# whose backend is unreachable otherwise waits query_wait_timeout (120 s).
probe() { PGPASSWORD=velox_test_app perl -e 'alarm 5; exec @ARGV' psql -h localhost -p 6432 -U velox_test_app -d velox_test -A -t -c "SELECT 1" >/dev/null 2>&1; }
for _ in $(seq 1 12); do
  if probe; then break; fi
  sleep 1
done
probe || { echo "pgbouncer not reachable"; docker logs "$NAME" 2>&1 | tail -20; exit 1; }
echo "pgbouncer up (transaction mode) -> ${DB_HOST}:5432; app pool via localhost:6432"

export TEST_ADMIN_DATABASE_URL="${TEST_ADMIN_DATABASE_URL:-postgres://velox:velox@localhost:5432/velox_test?sslmode=disable}"
export TEST_DATABASE_URL="postgres://velox_test_app:velox_test_app@localhost:6432/velox_test?sslmode=disable"
go test -p 1 $PKGS -count=1 -short=false
