#!/usr/bin/env bash
# Provoke the WAL-segment zero-fill stall on purpose, under two settings, so
# the mechanism is shown deterministically rather than waited for.
#
#   TARGET=aws ./provoke-walinit.sh <arm-label> <min_wal_size_MB> [rate] [batch] [idle_s] [duration]
#     e.g.  TARGET=aws ./provoke-walinit.sh provoke-stock 192
#           TARGET=aws ./provoke-walinit.sh provoke-6g   6144
#
# The recipe (why it works): after a quiet period the checkpointer's distance
# estimate decays, the next (small, timed) checkpoint REMOVES "surplus"
# recycled WAL segments down to min_wal_size, and when load resumes every new
# segment must be zero-filled by a committing backend under WALWriteLock — the
# tail event. So: apply the setting, sit idle for longer than
# checkpoint_timeout (300 s on RDS) so at least one small checkpoint runs, then
# drive the ingest rate for a few minutes with the sampler on and read back
# pg_wal size (CloudWatch TransactionLogsDiskUsage), the 1-second device writes
# (Enhanced Monitoring) and the k6 tail.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
LABEL="${1:?arm label}"; MINWAL="${2:?min_wal_size in MB}"; RATE="${3:-12000}"; BATCH="${4:-10}"; IDLE="${5:-420}"; DUR="${6:-4m}"
OUT="${OUT:-$HOME/.velox-bench-rig}"; REGION="${AWS_REGION:-ap-south-1}"
aws_() { aws --region "$REGION" "$@"; }
say() { printf '\n== [%s] %s  (%s)\n' "$LABEL" "$*" "$(date -u +%H:%M:%SZ)"; }
RES="$OUT/tail-$LABEL"; mkdir -p "$RES"

say "min_wal_size -> $MINWAL MB (dynamic, immediate)"
aws_ rds modify-db-parameter-group --db-parameter-group-name velox-bench-pg16 \
  --parameters "[{\"ParameterName\":\"min_wal_size\",\"ParameterValue\":\"$MINWAL\",\"ApplyMethod\":\"immediate\"}]" >/dev/null
sleep 30
live=$(TARGET=aws "$HERE/db-sampler.sh" settings 2>/dev/null | tail -1 | python3 -c "import sys,json; print(json.loads(sys.stdin.read())['min_wal_size'])")
[ "$live" = "$MINWAL" ] || { echo "FATAL: live min_wal_size=$live, wanted $MINWAL"; exit 1; }
echo "   live: min_wal_size=$live"

say "sampler on; idle ${IDLE}s so at least one small checkpoint runs (checkpoint_timeout=300)"
TARGET=aws "$HERE/db-sampler.sh" start "$LABEL" | tail -1
t_idle0=$(date -u +%s); sleep "$IDLE"

say "resume: $RATE ev/s batch $BATCH for $DUR (warmup 30 s discarded)"
( cd "$HERE" && TARGET=aws REPEATS=1 DURATION="$DUR" WARMUP=30s COOLDOWN=5 PROBE_GATE=0 CONFIGS="$LABEL:$RATE:$BATCH" ./measure.sh ) 2>&1 \
  | grep --line-buffered -vE "post-quantum|store now|openssh" | tee "$RES/measure.log" | grep --line-buffered -E "run [0-9]|passed|FAIL|results:" || true
MDIR=$(sed -n 's/^ *results: *//p' "$RES/measure.log" | head -1)

say "sampler off, fetch"
TARGET=aws "$HERE/db-sampler.sh" stop "$LABEL" | tail -1
TARGET=aws "$HERE/db-sampler.sh" fetch "$LABEL" "$RES"
[ -n "$MDIR" ] && [ -d "$MDIR" ] && cp "$MDIR"/* "$RES"/ 2>/dev/null || true
t_end=$(date -u +%s)

say "pg_wal size (CloudWatch TransactionLogsDiskUsage) across idle + resume"
aws_ cloudwatch get-metric-statistics --namespace AWS/RDS --metric-name TransactionLogsDiskUsage \
  --dimensions Name=DBInstanceIdentifier,Value=velox-bench-db \
  --start-time "$(python3 -c "import datetime as d;print(d.datetime.utcfromtimestamp($t_idle0-120).strftime('%Y-%m-%dT%H:%M:%SZ'))")" \
  --end-time "$(python3 -c "import datetime as d;print(d.datetime.utcfromtimestamp($t_end+60).strftime('%Y-%m-%dT%H:%M:%SZ'))")" \
  --period 60 --statistics Average --output json > "$RES/$LABEL.pgwal.json"
python3 - "$RES/$LABEL.pgwal.json" <<'PY'
import json,sys,datetime as dt
pts=sorted(json.load(open(sys.argv[1]))['Datapoints'],key=lambda p:p['Timestamp']); prev=None
for p in pts:
    v=p['Average']/1e9; t=dt.datetime.fromisoformat(p['Timestamp'].replace('Z','+00:00')).strftime('%H:%M')
    if prev is None or abs(v-prev)>0.05: print(f"   {t}Z pg_wal {v:.2f} GB" + ("" if prev is None else f" ({v-prev:+.2f})"))
    prev=v
PY
say "census"
python3 "$HERE/tail-census.py" "$RES" --series "$LABEL" | grep -E "buckets|worst|drops|pi_walwrite|pi_walinit|em_write>=|em_write_med|db_walinit|log_checkpoints|log_recycled|wal_MBps" || true
say "done -> $RES"
