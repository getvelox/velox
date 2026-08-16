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
ncust=$(psql "$DATABASE_URL" -qtA -c "SELECT count(*) FROM customers WHERE tenant_id='vlx_ten_bench'")
nmet=$(psql "$DATABASE_URL" -qtA -c "SELECT count(*) FROM meters WHERE tenant_id='vlx_ten_bench'")
echo "  spreading across $ncust customers and $nmet meters"
# Refuse the shape this file exists to avoid. Run velox-bench-seed first.
if [ "${ncust:-0}" -lt 2 ]; then
  echo "FATAL: only $ncust bench customer(s) exist — every row would land on one customer_id." >&2
  echo "       run velox-bench-seed (BENCH_CUSTOMERS defaults to 200) before seeding history." >&2
  exit 1
fi

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
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -c "SET app.livemode = 'off';
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
    -- The LATERAL subqueries MUST reference g. An UNCORRELATED lateral is
    -- evaluated ONCE per query, not once per row, so the earlier form picked a
    -- single random customer and gave it the entire 2M-row chunk — exactly the
    -- one-hot-edge shape the header of this file says it avoids. Measured: 5,000
    -- generated rows landed on 1 distinct customer of 51; with the correlation,
    -- 51. The AWS run's 5M seeded rows therefore sat on ~3 customers, not spread.
    CROSS JOIN LATERAL (
      SELECT id FROM customers WHERE tenant_id='vlx_ten_bench' AND g.g = g.g ORDER BY random() LIMIT 1
    ) c
    CROSS JOIN LATERAL (
      SELECT id FROM meters WHERE tenant_id='vlx_ten_bench' AND g.g = g.g ORDER BY random() LIMIT 1
    ) m;"
  done_rows=$(( done_rows + n ))
  echo "  $done_rows / $ROWS"
done

# ANALYZE, not VACUUM FULL: we want the planner to have current statistics, but
# leaving the table in its naturally-loaded state (bloat, page fill and all) is
# closer to a production table than a freshly rewritten one.
# Verify the rows landed where intended. Trusting the SET without checking is
# how the previous run produced a confidently-labelled wrong number.
wrong=$(psql "$DATABASE_URL" -A -t -c "SELECT count(*) FROM usage_events WHERE livemode = true")
if [ "${wrong:-0}" -gt 0 ]; then
  echo "FAIL: $wrong rows landed in livemode=true; the app.livemode session GUC did not take" >&2
  exit 1
fi
echo "verified: all seeded rows are livemode=false"

echo "analyzing"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -c "ANALYZE usage_events;"
psql "$DATABASE_URL" -A -t -c "
  SELECT 'rows=' || count(*) FROM usage_events;
  SELECT 'heap=' || pg_size_pretty(pg_table_size('usage_events'))
      || ' indexes=' || pg_size_pretty(pg_indexes_size('usage_events'));"
