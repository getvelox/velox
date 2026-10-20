#!/usr/bin/env bash
#
# quickstart.sh — start Velox on this machine with only Docker installed.
#
#   ./quickstart.sh            start Velox and create the first login
#   ./quickstart.sh demo       run the demo: token usage to a finalized invoice
#   ./quickstart.sh stop       stop Velox, keeping its data
#   ./quickstart.sh destroy    stop Velox and DELETE its data
#
# The dashboard is served on port 8080. To use another port:
#   VELOX_PORT=9000 ./quickstart.sh
#
# It pulls the published release images, so nothing is built from source and
# Go, Node and jq are not needed. To build from this checkout instead, run
# `./quickstart.sh --build`.
#
# This is for trying Velox on your own machine. It runs with APP_ENV=local, so
# the dashboard works over plain HTTP at http://localhost in every browser.
# For a server, follow README.md in this directory: it explains each setting
# this script fills in for you, and runs in production mode.

set -euo pipefail
cd "$(dirname "$0")"

# The release this script starts. Bumped with each release.
VELOX_VERSION="v0.9.0"
# The dashboard and API are served on this port. Override: VELOX_PORT=9000 ./quickstart.sh
PORT_DEFAULT="${VELOX_PORT:-8080}"
CREDS=quickstart-credentials.json

say()  { printf '%s\n' "$*"; }
fail() { printf 'quickstart: %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || fail "Docker is not installed. Get it from https://docs.docker.com/get-docker/"
docker compose version >/dev/null 2>&1 || fail "the Docker Compose plugin is missing (need 'docker compose', Docker 24 or newer)."
docker info >/dev/null 2>&1 || fail "Docker is not running, or this user cannot use it. Start Docker and run this again. On Linux, add yourself to the docker group (then log out and back in), or run this with sudo."

# 32 random bytes as 64 hex characters, without needing openssl.
rand_hex() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

# Run a request from inside the stack, so the host needs no curl.
in_stack() { docker compose exec -T nginx "$@"; }

port() { sed -n 's/^NGINX_HTTP_PORT=//p' .env 2>/dev/null | tail -1; }

write_env() {
  local build="$1"
  [ -f .env ] && return 0
  umask 077
  {
    echo "# Written by quickstart.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ). Keep this file private."
    echo "POSTGRES_PASSWORD=$(rand_hex 24)"
    echo "VELOX_APP_DB_PASSWORD=$(rand_hex 24)"
    echo "VELOX_ENCRYPTION_KEY=$(rand_hex 32)"
    echo "VELOX_BOOTSTRAP_TOKEN=$(rand_hex 32)"
    # Its own project name, so it never shares containers or a data volume
    # with another Velox stack on this machine (a development checkout, or an
    # install made by hand from this directory).
    echo "COMPOSE_PROJECT_NAME=velox-quickstart"
    # Local mode, because this install is served over plain HTTP. Production
    # mode always marks the sign-in cookie Secure, and Safari does not keep a
    # Secure cookie at http://localhost, so sign-in would fail there.
    echo "APP_ENV=local"
    echo "NGINX_HTTP_PORT=${PORT_DEFAULT}"
    # Where Stripe returns a customer after they save a card.
    echo "CUSTOMER_PORTAL_URL=http://localhost:${PORT_DEFAULT}"
    if [ "$build" = no ]; then
      echo "VELOX_IMAGE=ghcr.io/getvelox/velox:${VELOX_VERSION}"
      echo "VELOX_DASHBOARD_IMAGE=ghcr.io/getvelox/velox-dashboard:${VELOX_VERSION}"
    fi
  } > .env
  say "Wrote .env with four new secrets."
}

wait_healthy() {
  local i
  for i in $(seq 1 90); do
    if in_stack wget -qO- http://velox-api:8080/health >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  docker compose logs --tail 30 velox-api >&2 || true
  fail "Velox did not become healthy in three minutes. The last log lines are above."
}

json_field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" "$CREDS" | head -1; }

bootstrap() {
  if [ -s "$CREDS" ]; then return 0; fi
  local token reply err status errf
  token=$(sed -n 's/^VELOX_BOOTSTRAP_TOKEN=//p' .env | tail -1)
  errf=$(mktemp)
  # busybox wget prints the body of a 2xx only. On any other status it
  # prints "server returned error: HTTP/1.1 <code> <reason>" to stderr.
  if reply=$(in_stack wget -qO- \
      --header "Authorization: Bearer ${token}" \
      --header "Content-Type: application/json" \
      --post-data '{"tenant_name":"My company"}' \
      http://velox-api:8080/v1/bootstrap 2>"$errf"); then
    rm -f "$errf"
    umask 077
    printf '%s\n' "$reply" > "$CREDS"
    return 0
  fi
  err=$(tr -d '\r' < "$errf" | head -3); rm -f "$errf"
  status=$(printf '%s\n' "$err" | sed -n 's/.*server returned error: HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' | head -1)
  say ""
  if [ "$status" = 409 ]; then
    # 409 is the only answer that means a first login already exists.
    say "The first login was created earlier, and its password is shown only once."
    say "If you have lost it, run './quickstart.sh destroy' and start again."
    return 1
  fi
  # Anything else (a rate limit, a server error, no answer) created nothing:
  # the first login is made in one transaction, so trying again is safe.
  say "Velox could not create the first login: ${err:-no reply from Velox}"
  if [ "$status" = 429 ]; then
    say "Velox is limiting requests. Wait a minute, then run ./quickstart.sh again."
  else
    say "See 'docker compose logs velox-api' for the cause, then run ./quickstart.sh again."
  fi
  say "Your data is kept. Do not run destroy for this."
  exit 1
}

show_login() {
  local p; p=$(port); p=${p:-$PORT_DEFAULT}
  say ""
  say "Velox is running."
  say ""
  say "  Dashboard   http://localhost:${p}"
  say "  Email       $(json_field owner_email)"
  say "  Password    $(json_field owner_password)"
  say "  Test key    $(json_field secret_key_test)"
  say ""
  say "Open the dashboard address in your browser and sign in with this email and"
  say "password. The password is shown only once. The dashboard opens in test mode,"
  say "where no real money moves."
  say ""
  say "These are also saved in deploy/compose/${CREDS}. Keep that file private:"
  say "it also holds a live key, which can charge real cards once Stripe is connected."
  say ""
  say "Next: ./quickstart.sh demo"
  say "  It bills a demo customer for one month of AI token usage in about two"
  say "  minutes, then prints links to the invoice and the customer."
  say "  What each demo step does: docs/quick-start.md"
}

cmd_up() {
  local build="$1"
  write_env "$build"
  if [ "$build" = yes ]; then
    say "Building Velox from this checkout. This takes a few minutes the first time."
    docker compose up -d --build
  else
    say "Starting Velox ${VELOX_VERSION} from the published images."
    say "The first start downloads them, which can take a few minutes."
    docker compose up -d --no-build
  fi || fail "Docker could not start the stack. If the message above says a port is already in use, run './quickstart.sh destroy', then 'VELOX_PORT=9000 ./quickstart.sh'."
  say "Waiting for Velox to be ready..."
  wait_healthy
  if bootstrap; then show_login; fi
}

cmd_demo() {
  [ -s "$CREDS" ] || fail "run ./quickstart.sh first."
  docker compose ps --status running --quiet nginx | grep -q . || fail "Velox is not running. Run ./quickstart.sh first."
  local key net p
  key=$(json_field secret_key_test)
  net=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}' "$(docker compose ps -q nginx)")
  p=$(port); p=${p:-$PORT_DEFAULT}
  # The demo script needs bash, curl and jq. Run it in a small container on
  # the stack's network so the host needs none of them.
  docker run --rm --network "$net" \
    -v "$(cd ../.. && pwd)/scripts:/scripts:ro" \
    -e VELOX_BASE=http://nginx \
    -e VELOX_DASHBOARD_URL="http://localhost:${p}" \
    -e VELOX_PUBLIC_URL="http://localhost:${p}" \
    alpine:3.22 sh -c 'apk add --no-cache -q bash curl jq coreutils >/dev/null && bash /scripts/demo.sh "$0"' "$key"
}

case "${1:-up}" in
  up)        cmd_up no ;;
  --build)   cmd_up yes ;;
  demo)      cmd_demo ;;
  stop)      docker compose stop; say "Stopped. Your data is kept. Run ./quickstart.sh to start again." ;;
  destroy)
    docker compose down -v
    rm -f "$CREDS" .env
    say "Stopped, and the data of this quickstart install is deleted. Other Velox installs on this machine are not touched." ;;
  -h|--help|help) sed -n '3,15p' "$0" | sed 's/^# \{0,1\}//' ;;
  *) fail "unknown command '$1'. Try: ./quickstart.sh --help" ;;
esac
