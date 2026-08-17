#!/usr/bin/env bash
# The database's OWN ceiling for Velox's row shape — the denominator every
# Velox throughput figure has been missing.
#
# "10,203 events/sec" means nothing on its own: is that 30% of what the
# database can commit, or 95%? pgbench is the Postgres community's standard
# instrument for exactly this question, and running it on the SAME database,
# with the SAME row shape, decomposes Velox's number into three parts a buyer
# can reason about:
#
#   leg A  commit floor      BEGIN; INSERT one usage_events row; COMMIT — with
#                            the RLS session variables set ONCE per connection.
#                            This is what the hardware + Postgres can do for
#                            our row, our five indexes, our trigger. Nothing
#                            above it is Velox's fault.
#   leg B  velox tx protocol BEGIN; set_config x3; INSERT; COMMIT — the exact
#                            per-transaction protocol Velox's store uses (three
#                            set_config round trips for row-level security).
#                            A minus B is what RLS-per-transaction costs.
#   Velox measured ev/s      from measure.sh. B minus this is what HTTP + auth
#                            + resolve + the Go service cost.
#
# BATCH=N inserts N rows per transaction (multi-row VALUES), which is the
# DB-side analogue of the batch endpoint and shows how much of batching's win
# is simply commits-per-event.
#
# Every leg VERIFIES its own row count: pgbench's "transactions processed"
# times BATCH must equal the delta in usage_events, or the leg is reported as
# NOT VERIFIED. pgbench's TPS is client-side too.
#
#   DATABASE_URL=postgres://... ./db-ceiling.sh              # both legs
#   DATABASE_URL=... CLIENTS=16 DURATION=60 BATCH=10 ./db-ceiling.sh
#   DATABASE_URL=... VELOX_EVS=10203 ./db-ceiling.sh          # prints the ratios
#   TARGET=aws VELOX_EVS=10203 ./db-ceiling.sh                # on the rig: runs itself on the app node
#
# Needs pgbench (postgresql16-contrib on AL2023; libpq on macOS) and the bench
# fixtures from velox-bench-seed (it spreads rows across those customers).
set -uo pipefail

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
  exec ssh -o StrictHostKeyChecking=no -i "$KEYFILE" "ec2-user@$APP_PUB" \
    "TARGET=local DATABASE_URL='$REMOTE_DSN' CLIENTS='${CLIENTS:-16}' DURATION='${DURATION:-60}' BATCH='${BATCH:-1}' VELOX_EVS='${VELOX_EVS:-}' LIVEMODE='${LIVEMODE:-on}' ROUNDS='${ROUNDS:-2}' bash -s -- $*" < "$0"
fi

: "${DATABASE_URL:?set DATABASE_URL (admin/owner role — pgbench inserts directly)}"
CLIENTS="${CLIENTS:-16}"
DURATION="${DURATION:-60}"
BATCH="${BATCH:-1}"
VELOX_EVS="${VELOX_EVS:-}"
LIVEMODE="${LIVEMODE:-on}"          # must match how the bench fixtures were seeded
TENANT="vlx_ten_bench"

command -v pgbench >/dev/null || { echo "FATAL: pgbench not installed (postgresql16-contrib / libpq)"; exit 1; }
say()  { printf '\n== %s\n' "$*"; }
info() { printf '   %s\n' "$*"; }
q()    { psql "$DATABASE_URL" -qtA -v ON_ERROR_STOP=1 -c "$1"; }

ncust=$(q "SELECT count(*) FROM customers WHERE tenant_id='$TENANT'")
[ "${ncust:-0}" -ge 1 ] || { echo "FATAL: no bench customers — run velox-bench-seed first"; exit 1; }
# LIVEMODE must match the fixtures, or the trigger files every row under the
# other partition. Refuse rather than measure the wrong table.
fixmode=$(q "SELECT CASE WHEN bool_and(livemode) THEN 'on' WHEN bool_and(NOT livemode) THEN 'off' ELSE 'mixed' END FROM customers WHERE tenant_id='$TENANT'")
if [ "$fixmode" != "$LIVEMODE" ]; then
  echo "FATAL: LIVEMODE=$LIVEMODE but the bench fixtures are livemode=$fixmode — rows would land in the other partition." >&2
  echo "       pass LIVEMODE=$fixmode (or re-seed with BENCH_LIVEMODE)." >&2
  exit 1
fi
maxc=$((ncust - 1))

WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT

# Velox's row shape: the same columns the store's INSERT writes. Customer ids
# come from the ACTUAL seeded set (an array literal built once here), not from
# an assumed 'prefix + 000..n-1' pattern — a database holding an older bench
# customer alongside the seeded ones made that assumption pick a non-existent
# id and every insert failed on the foreign key. BATCH rows per statement via
# generate_series; unique idempotency keys; ADR-044-shaped dims.
IDS_SQL=$(q "SELECT 'ARRAY[' || string_agg(quote_literal(id), ',') || ']' FROM customers WHERE tenant_id='$TENANT'")
INSERT_SQL="INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, properties, idempotency_key, timestamp, origin, provider_cost_micros, provider_cost_source)
SELECT 'vlx_evt_pgb_' || gen_random_uuid()::text, '$TENANT',
       (${IDS_SQL})[1 + floor(random() * $ncust)::int],
       'vlx_mtr_bench', (floor(random() * 1000) + 1)::numeric,
       jsonb_build_object('model', (ARRAY['gpt-4','claude-3-opus','gemini-pro','llama-2-70b','mistral-large'])[1 + floor(random()*5)],
                          'operation', (ARRAY['input','output','embedding','moderation'])[1 + floor(random()*4)],
                          'cached', random() < 0.5),
       'pgb-' || gen_random_uuid()::text, now(), 'api', NULL, 'not_applicable'
FROM generate_series(1, $BATCH) g;"

# leg A: session-level GUCs (set once by PGOPTIONS), bare tx around the insert
cat > "$WORK/legA.sql" <<SQL
BEGIN;
$INSERT_SQL
COMMIT;
SQL
# leg B: Velox's per-transaction protocol — three set_config round trips
cat > "$WORK/legB.sql" <<SQL
BEGIN;
SELECT set_config('app.bypass_rls', 'off', true);
SELECT set_config('app.tenant_id', '$TENANT', true);
SELECT set_config('app.livemode', '$LIVEMODE', true);
$INSERT_SQL
COMMIT;
SQL

run_leg() { # name script
  local name=$1 script=$2 before after tx tps rows
  # Count in the INTENDED partition, not the whole tenant: the set_livemode
  # trigger places rows by the app.livemode GUC, so a mismatch between LIVEMODE
  # and the fixtures' mode writes every row to the OTHER partition — and a
  # tenant-wide count still verifies. Measured: 17,818 rows silently landed in
  # livemode=false during a LIVEMODE=off run against live fixtures.
  local lm; [ "$LIVEMODE" = "on" ] && lm=true || lm=false
  before=$(q "SELECT count(*) FROM usage_events WHERE tenant_id='$TENANT' AND livemode=$lm")
  # PGOPTIONS sets the RLS/livemode GUCs at SESSION level for leg A; harmless
  # for leg B, whose per-tx set_config then overrides them the way Velox does.
  PGOPTIONS="-c app.tenant_id=$TENANT -c app.livemode=$LIVEMODE -c app.bypass_rls=off" \
    pgbench "$DATABASE_URL" -n -M prepared -c "$CLIENTS" -j "$(( CLIENTS < 8 ? CLIENTS : 8 ))" \
      -T "$DURATION" -f "$script" > "$WORK/$name.out" 2>&1 || { echo "  pgbench FAILED:"; tail -5 "$WORK/$name.out" | sed 's/^/    /'; return 1; }
  after=$(q "SELECT count(*) FROM usage_events WHERE tenant_id='$TENANT' AND livemode=$lm")
  tx=$(sed -n 's/^number of transactions actually processed: \([0-9]*\).*/\1/p' "$WORK/$name.out")
  tps=$(sed -n 's/^tps = \([0-9.]*\).*/\1/p' "$WORK/$name.out" | head -1)
  rows=$((after - before))
  evs=$(awk -v t="$tps" -v b="$BATCH" 'BEGIN{printf "%.0f", t*b}')
  if [ "$rows" = "$((tx * BATCH))" ]; then ver="VERIFIED (rows == tx x batch)"; else ver="** NOT VERIFIED: rows $rows != tx $tx x $BATCH **"; fi
  printf '   %-22s %8s tx/s  = %8s rows/s   %s\n' "$name" "$tps" "$evs" "$ver"
  echo "$evs" > "$WORK/$name.evs"
}

# ROUNDS: the legs are INTERLEAVED (A B A B ...) and the median of each is
# reported. Run once each, back to back, the comparison is confounded by table
# state: on the real rig, straight after a 25k-rows/s closed-loop burst, leg A
# ate the checkpoint/autovacuum churn and leg B came out 53% FASTER — an
# impossible protocol cost, and exactly what interleaving cancels.
ROUNDS="${ROUNDS:-2}"
say "db-ceiling: $CLIENTS clients, ${DURATION}s per leg, batch $BATCH, $ncust customers, livemode=$LIVEMODE, $ROUNDS interleaved rounds"
q "SELECT 'table: '||count(*)||' rows, '||pg_size_pretty(pg_table_size('usage_events'))||' heap, '||pg_size_pretty(pg_indexes_size('usage_events'))||' idx' FROM usage_events" | sed 's/^/   /'
: > "$WORK/A.all"; : > "$WORK/B.all"
for r in $(seq 1 "$ROUNDS"); do
  info "round $r"
  run_leg "A commit-floor"  "$WORK/legA.sql";      cat "$WORK/A commit-floor.evs"      >> "$WORK/A.all" 2>/dev/null; echo >> "$WORK/A.all"
  run_leg "B velox-tx-protocol" "$WORK/legB.sql";  cat "$WORK/B velox-tx-protocol.evs" >> "$WORK/B.all" 2>/dev/null; echo >> "$WORK/B.all"
done
median() { sort -n | awk '{a[NR]=$1} END{ if (NR==0) print ""; else if (NR%2==1) print a[(NR+1)/2]; else printf "%.0f\n", (a[NR/2]+a[NR/2+1])/2 }'; }
a=$(grep -E '^[0-9]+$' "$WORK/A.all" | median); b=$(grep -E '^[0-9]+$' "$WORK/B.all" | median)
info "medians over $ROUNDS rounds: A $a rows/s, B $b rows/s"
say "decomposition (median rows/s over $ROUNDS interleaved rounds, batch $BATCH)"
if [ -n "$a" ] && [ -n "$b" ]; then
  awk -v a="$a" -v b="$b" 'BEGIN{printf "   RLS per-tx protocol costs: %.0f%% of the commit floor  (A %s -> B %s)\n", (1-b/a)*100, a, b}'
fi
if [ -n "$VELOX_EVS" ] && [ -n "$b" ]; then
  awk -v v="$VELOX_EVS" -v a="$a" -v b="$b" 'BEGIN{
    printf "   Velox at %s ev/s is %.0f%% of the DB commit floor and %.0f%% of the same protocol without HTTP/app\n", v, v/a*100, v/b*100
    printf "   -> HTTP + auth + resolve + service overhead: %.0f%% of B\n", (1-v/b)*100 }'
  info "(only meaningful if VELOX_EVS came from a closed-loop run at BATCH=$BATCH — a mismatched batch makes these fractions nonsense)"
else
  info "pass VELOX_EVS=<measured ev/s at this batch> to see Velox as a fraction of the ceiling"
fi
