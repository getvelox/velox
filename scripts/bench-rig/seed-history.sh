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
#
# Rows are spread across every seeded customer and meter, with timestamps
# scattered over the trailing 30 days, so the btrees are not all appending to
# one hot right-hand edge — which is what a single-customer seed would do and
# is the easiest case for Postgres.
set -euo pipefail

ROWS="${1:-20000000}"
: "${DATABASE_URL:?set DATABASE_URL}"
CHUNK="${CHUNK:-2000000}"

echo "seeding $ROWS usage_events in chunks of $CHUNK"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -c "
  SELECT count(*) AS customers FROM customers WHERE tenant_id='vlx_ten_bench';
  SELECT count(*) AS meters    FROM meters    WHERE tenant_id='vlx_ten_bench';"

done_rows=0
while [ "$done_rows" -lt "$ROWS" ]; do
  n=$(( ROWS - done_rows )); [ "$n" -gt "$CHUNK" ] && n=$CHUNK
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -c "
    INSERT INTO usage_events (tenant_id, customer_id, meter_id, quantity, properties, timestamp, livemode)
    SELECT 'vlx_ten_bench',
           c.id,
           m.id,
           (random()*1000)::bigint + 1,
           jsonb_build_object(
             'model',     (ARRAY['gpt-4','claude-3-opus','gemini-pro','llama-2-70b','mistral-large'])[1+floor(random()*5)],
             'operation', (ARRAY['input','output','embedding','moderation'])[1+floor(random()*4)],
             'cached',    (random() < 0.5)
           ),
           now() - (random() * interval '30 days'),
           false
    FROM generate_series(1, $n) g
    CROSS JOIN LATERAL (
      SELECT id FROM customers WHERE tenant_id='vlx_ten_bench' ORDER BY random() LIMIT 1
    ) c
    CROSS JOIN LATERAL (
      SELECT id FROM meters WHERE tenant_id='vlx_ten_bench' ORDER BY random() LIMIT 1
    ) m;"
  done_rows=$(( done_rows + n ))
  echo "  $done_rows / $ROWS"
done

# ANALYZE, not VACUUM FULL: we want the planner to have current statistics, but
# leaving the table in its naturally-loaded state (bloat, page fill and all) is
# closer to a production table than a freshly rewritten one.
echo "analyzing"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -c "ANALYZE usage_events;"
psql "$DATABASE_URL" -A -t -c "
  SELECT 'rows=' || count(*) FROM usage_events;
  SELECT 'heap=' || pg_size_pretty(pg_table_size('usage_events'))
      || ' indexes=' || pg_size_pretty(pg_indexes_size('usage_events'));"
