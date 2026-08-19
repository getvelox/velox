#!/usr/bin/env bash
# One instrumented sustained series, control or treatment, run identically:
#
#   TARGET=aws ./tail-series.sh <label> <rate> <batch> [repeats]
#     e.g.  TARGET=aws ./tail-series.sh ctrl-12k 12000 10 5
#
# 1. TRUNCATE usage_events, reseed SEED_ROWS (20M), settle SETTLE s (600) —
#    with the DB sampler already running, so the post-seed autovacuum is on
#    the record too;
# 2. measure.sh SUSTAINED (repeats x 10 min) with the sampler running through
#    the cool-downs (a tail event's aftermath lands in the next repeat's first
#    minute — the second AWS run's E2 did exactly that);
# 3. stop + fetch samples, pg_stat_statements before/after, the RDS postgres
#    log for the window; run analyze-tail.py and pgbadger over it.
#
# Everything lands in one results directory, named by label, so a treatment
# series is compared to its control by diffing two directories.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
LABEL="${1:?label}"; RATE="${2:?rate}"; BATCH="${3:?batch}"; REPEATS="${4:-5}"
SEED_ROWS="${SEED_ROWS:-20000000}"; SETTLE="${SETTLE:-600}"
OUT="${OUT:-$HOME/.velox-bench-rig}"
[ "${TARGET:-}" = "aws" ] || { echo "TARGET=aws only (this drives the rig)"; exit 1; }
REGION="${AWS_REGION:-ap-south-1}"
aws_() { aws --region "$REGION" "$@"; }
KEYFILE="$OUT/velox-bench-key.pem"
APP_PUB=$(aws_ ec2 describe-instances --filters "Name=tag:Name,Values=velox-bench-app" "Name=instance-state-name,Values=running" --query 'Reservations[].Instances[0].PublicIpAddress' --output text)
DBHOST=$(aws_ rds describe-db-instances --db-instance-identifier velox-bench-db --query 'DBInstances[0].Endpoint.Address' --output text)
DBPASS=$(cat "$OUT/db-password")
app_psql() { ssh -o StrictHostKeyChecking=no -o IdentitiesOnly=yes -o LogLevel=ERROR -o ServerAliveInterval=20 -o ServerAliveCountMax=3 -i "$KEYFILE" "ec2-user@$APP_PUB" "PGPASSWORD='$DBPASS' psql -h $DBHOST -U velox -d velox -X -qtA -c \"$1\""; }
say() { printf '\n== [%s] %s  (%s)\n' "$LABEL" "$*" "$(date -u +%H:%M:%SZ)"; }
RES="$OUT/tail-$LABEL"; mkdir -p "$RES"

# The security group admits ssh from the IP provision.sh saw. A laptop on a
# dynamic address changes IP mid-series (it did, 40 minutes into the first
# control run: the seed's ssh session was severed and the driver hung on a
# dead pipe). Make sure the CURRENT address is admitted before every step.
ensure_my_ip() {
  local ip sg; ip="$(curl -s https://checkip.amazonaws.com | tr -d '[:space:]')/32"
  sg=$(aws_ ec2 describe-security-groups --filters Name=group-name,Values=velox-bench-sg --query 'SecurityGroups[0].GroupId' --output text)
  if ! aws_ ec2 describe-security-groups --group-ids "$sg" --query 'SecurityGroups[0].IpPermissions[?FromPort==`22`].IpRanges[].CidrIp' --output text | grep -qF "$ip"; then
    aws_ ec2 authorize-security-group-ingress --group-id "$sg" --protocol tcp --port 22 --cidr "$ip" >/dev/null && echo "   (ssh admitted from $ip)"
  fi
}
ensure_my_ip

say "settings on record"
TARGET=aws "$HERE/db-sampler.sh" settings > "$RES/settings.json"
python3 -c "import json,sys; L=open('$RES/settings.json').read().strip().split('\n'); d=json.loads(L[-1]); print('   ' + ', '.join(f'{k}={d[k]}' for k in ('max_wal_size','min_wal_size','checkpoint_timeout','autovacuum_naptime','log_autovacuum_min_duration','log_min_duration_statement','deadlock_timeout','wal_segment_size','shared_buffers','gin_pending_list_limit') if k in d))"

if [ "${SKIP_SEED:-0}" = "1" ]; then
  say "SKIP_SEED=1: sampler assumed running, table assumed seeded + settled ($(app_psql 'SELECT count(*) FROM usage_events') rows)"
else
  say "sampler on, then TRUNCATE + reseed $SEED_ROWS + settle ${SETTLE}s"
  TARGET=aws "$HERE/db-sampler.sh" start "$LABEL"
  app_psql "TRUNCATE usage_events" >/dev/null
  TARGET=aws "$HERE/seed-history.sh" "$SEED_ROWS" 2>&1 | grep -vE "post-quantum|store now|openssh" | tail -4
  echo "   settling ${SETTLE}s (post-seed autovacuum, on the record)"; sleep "$SETTLE"
fi
ensure_my_ip

say "measure: $RATE ev/s batch $BATCH, $REPEATS x 10 min"
( cd "$HERE" && TARGET=aws SUSTAINED=1 REPEATS="$REPEATS" PROBE_GATE=0 CONFIGS="$LABEL:$RATE:$BATCH" ./measure.sh ) 2>&1 | grep --line-buffered -vE "post-quantum|store now|openssh" | tee "$RES/measure.log" | grep --line-buffered -E "run [0-9]|passed|FAIL|results:" || true
MDIR=$(sed -n 's/^ *results: *//p' "$RES/measure.log" | head -1)

say "sampler off, fetch evidence"
TARGET=aws "$HERE/db-sampler.sh" stop "$LABEL" | tail -1
TARGET=aws "$HERE/db-sampler.sh" fetch "$LABEL" "$RES"
[ -n "$MDIR" ] && [ -d "$MDIR" ] && { cp "$MDIR"/* "$RES"/ 2>/dev/null || true; echo "   measure results copied from $MDIR"; }

say "analysis"
python3 "$HERE/analyze-tail.py" "$RES" --series "$LABEL" > "$RES/analysis.txt" 2>&1 || true
grep -E "^## |TAIL WINDOW|log census" "$RES/analysis.txt" | head -40
if command -v pgbadger >/dev/null && [ -s "$RES/$LABEL.postgres.log" ]; then
  pgbadger -q --prefix '%t:%r:%u@%d:[%p]:' -o "$RES/pgbadger.html" "$RES/$LABEL.postgres.log" >/dev/null 2>&1 \
    && echo "   pgbadger report: $RES/pgbadger.html" || echo "   (pgbadger failed)"
fi
say "done -> $RES"
