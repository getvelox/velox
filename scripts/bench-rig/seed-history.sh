#!/usr/bin/env bash
# Bulk-loads historical usage_events so the ingest benchmark measures writes
# into a REALISTIC table rather than an empty one.
#
# Why this exists: every ingest number we published before this was measured
# against a freshly truncated table. That is the optimistic case and we never
# said so. Index maintenance grows with volume — usage_events carries five
# indexes including a GIN over the properties JSONB and a UNIQUE over
# (tenant_id, livemode, idempotency_key) — and none of that cost shows up when
# the table and its indexes fit comfortably in cache.
#
#   DATABASE_URL=... ./seed-history.sh 20000000
#   TARGET=aws ./seed-history.sh 20000000     # on the rig: runs itself on the app node
#
# Rows are spread across every seeded customer and meter, with timestamps
# scattered over the trailing 30 days, so the btrees are not all appending to
# one hot right-hand edge — which is what a single-customer seed would do and
# is the easiest case for Postgres.
set -euo pipefail

# ---------------------------------------------------------------------------
# TARGET=aws: RDS is --no-publicly-accessible, so this script cannot reach the
# database from a laptop. Resolve the rig and re-run THIS script on the app
# node over ssh (piped over stdin — nothing to copy first), with the same
# environment. The app node has psql and pgbench (postgresql16-contrib).
# ---------------------------------------------------------------------------
if [ "${TARGET:-local}" = "aws" ]; then
  REGION="${AWS_REGION:-ap-south-1}"; OUT="${OUT:-$HOME/.velox-bench-rig}"
  KEYFILE="$OUT/velox-bench-key.pem"; [ -s "$KEYFILE" ] || { echo "FATAL: no key at $KEYFILE"; exit 1; }
  aws_() { aws --region "$REGION" "$@"; }
  APP_PUB=$(aws_ ec2 describe-instances --filters "Name=tag:Name,Values=velox-bench-app" "Name=instance-state-name,Values=running" \
    --query 'Reservations[].Instances[0].PublicIpAddress' --output text)
  [ -n "$APP_PUB" ] && [ "$APP_PUB" != "None" ] || { echo "FATAL: app instance not running"; exit 1; }
  DBHOST=$(aws_ rds describe-db-instances --db-instance-identifier velox-bench-db --query 'DBInstances[0].Endpoint.Address' --output text)
  DBPASS=$(cat "$OUT/db-password")
  REMOTE_DSN="postgres://velox:$DBPASS@$DBHOST:5432/${DBNAME:-velox}?sslmode=require"
  echo "== running on the app node ($APP_PUB) against $DBHOST"
  # Forward every knob this script honours; TARGET is dropped so it runs locally there.
  exec ssh -o StrictHostKeyChecking=no -o ServerAliveInterval=20 -o ServerAliveCountMax=3 -i "$KEYFILE" "ec2-user@$APP_PUB" \
    "TARGET=local DATABASE_URL='$REMOTE_DSN' CHUNK='${CHUNK:-2000000}' bash -s -- $*" < "$0"
fi

ROWS="${1:-20000000}"
: "${DATABASE_URL:?set DATABASE_URL}"
CHUNK="${CHUNK:-2000000}"

echo "seeding $ROWS usage_events in chunks of $CHUNK"
ncust=$(psql "$DATABASE_URL" -qtA -c "SELECT count(*) FROM customers WHERE tenant_id='vlx_ten_bench'")
nmet=$(psql "$DATABASE_URL" -qtA -c "SELECT count(*) FROM meters WHERE tenant_id='vlx_ten_bench'")
echo "  spreading across $ncust customers and $nmet meters"
# Refuse the shape this file exists to avoid. Run velox-bench-seed first.
if [ "${ncust:-0}" -lt 2 ]; then
  echo "FATAL: only $ncust bench customer(s) exist — every row would land on one customer_id." >&2
  echo "       run velox-bench-seed (BENCH_CUSTOMERS defaults to 200) before seeding history." >&2
  exit 1
fi
# Seed into the SAME partition the fixtures live in. velox-bench-seed defaults
# to live mode now; an earlier version of this script hard-coded 'off' and
# put 20M rows of "history" in the partition the bench key could never see.
fixmode=$(psql "$DATABASE_URL" -qtA -c "SELECT CASE WHEN bool_and(livemode) THEN 'on' WHEN bool_and(NOT livemode) THEN 'off' ELSE 'mixed' END FROM customers WHERE tenant_id='vlx_ten_bench'")
case "$fixmode" in on|off) ;; *) echo "FATAL: bench customers are in mixed livemodes ($fixmode) — re-seed" >&2; exit 1;; esac
[ "$fixmode" = "on" ] && LM_BOOL=true || LM_BOOL=false
echo "  livemode partition: $fixmode (matching the fixtures)"
before_rows=$(psql "$DATABASE_URL" -qtA -c "SELECT count(*) FROM usage_events WHERE tenant_id='vlx_ten_bench' AND livemode=$LM_BOOL")

done_rows=0
while [ "$done_rows" -lt "$ROWS" ]; do
  n=$(( ROWS - done_rows )); [ "$n" -gt "$CHUNK" ] && n=$CHUNK
  # livemode is NOT taken from the INSERT. A BEFORE trigger overwrites it:
  #   NEW.livemode := (current_setting('app.livemode', true) IS DISTINCT FROM 'off')
  # so rows default to livemode=true unless the SESSION sets app.livemode='off'.
  # Writing `false` in the column list is silently ignored — an earlier run of
  # this script seeded 5M rows into the wrong partition that way, and the
  # benchmark then measured a table it did not think it was measuring.
  # The SET must be in the same psql invocation as the INSERT to share a session.
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -c "SET app.livemode = '$fixmode';
    INSERT INTO usage_events (tenant_id, customer_id, meter_id, quantity, properties, timestamp)
    SELECT 'vlx_ten_bench',
           c.id,
           m.id,
           (random()*1000)::bigint + 1,
           jsonb_build_object(
             'model',     (ARRAY['gpt-4','claude-3-opus','gemini-pro','llama-2-70b','mistral-large'])[1+floor(random()*5)],
             'operation', (ARRAY['input','output','embedding','moderation'])[1+floor(random()*4)],
             'cached',    (random() < 0.5)
           ),
           now() - (random() * interval '30 days')
    FROM generate_series(1, $n) g
    -- Customer and meter ids are picked from arrays built ONCE per statement
    -- and indexed by random(): O(1) per row. The previous form was a correlated
    -- LATERAL 'ORDER BY random() LIMIT 1' per row — a 200-row sort for every one
    -- of 2M rows, ~8 minutes per chunk on RDS. (Its uncorrelated predecessor was
    -- worse: evaluated once per statement, it put every chunk on ONE customer.)
    CROSS JOIN (SELECT array_agg(id) AS ids FROM customers WHERE tenant_id='vlx_ten_bench') ca
    CROSS JOIN (SELECT array_agg(id) AS ids FROM meters    WHERE tenant_id='vlx_ten_bench') ma
    CROSS JOIN LATERAL (SELECT ca.ids[1 + floor(random() * array_length(ca.ids, 1))::int] AS id WHERE g.g = g.g) c
    CROSS JOIN LATERAL (SELECT ma.ids[1 + floor(random() * array_length(ma.ids, 1))::int] AS id WHERE g.g = g.g) m;"
  done_rows=$(( done_rows + n ))
  echo "  $done_rows / $ROWS"
done

# ANALYZE, not VACUUM FULL: we want the planner to have current statistics, but
# leaving the table in its naturally-loaded state (bloat, page fill and all) is
# closer to a production table than a freshly rewritten one.
# Verify the rows landed where intended. Trusting the SET without checking is
# how the previous run produced a confidently-labelled wrong number.
after_rows=$(psql "$DATABASE_URL" -qtA -c "SELECT count(*) FROM usage_events WHERE tenant_id='vlx_ten_bench' AND livemode=$LM_BOOL")
gained=$((after_rows - before_rows))
if [ "$gained" -ne "$ROWS" ]; then
  echo "FAIL: asked for $ROWS rows in livemode=$fixmode, the partition gained $gained — the app.livemode session GUC did not take, or rows went to the other partition" >&2
  exit 1
fi
spread=$(psql "$DATABASE_URL" -qtA -c "SELECT count(DISTINCT customer_id) FROM usage_events WHERE tenant_id='vlx_ten_bench' AND livemode=$LM_BOOL AND idempotency_key IS NULL")
echo "verified: $gained rows landed in livemode=$fixmode, spread over $spread customers"

echo "analyzing"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -c "ANALYZE usage_events;"
psql "$DATABASE_URL" -A -t -c "
  SELECT 'rows=' || count(*) FROM usage_events;
  SELECT 'heap=' || pg_size_pretty(pg_table_size('usage_events'))
      || ' indexes=' || pg_size_pretty(pg_indexes_size('usage_events'));"
