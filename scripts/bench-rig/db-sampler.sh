#!/usr/bin/env bash
# Samples what the DATABASE is doing while a benchmark runs — every 5 seconds,
# one JSON line, from the app node — so a latency-tail event can be attributed
# to a mechanism instead of a guess.
#
# Why this exists: in the second AWS run two repeats failed on a tail event
# (p99 up 10-30x for ~2 minutes, p50 flat) with storage, CPU and memory all
# flat in CloudWatch. CloudWatch is a 60 s view of the volume; it cannot see a
# checkpoint's fsync phase, an autovacuum walking the heap, a GIN pending-list
# flush, buffer eviction pressure, or backends stacking on a lock. Postgres
# can, and says so in its statistics views and its log. This captures both.
#
#   TARGET=aws ./db-sampler.sh start <tag>     # runs on the app node, writes /tmp/dbsample-<tag>.jsonl
#   TARGET=aws ./db-sampler.sh stop  <tag>
#   TARGET=aws ./db-sampler.sh fetch <tag> <dir> # copies the samples + pg_stat_statements
#                                                # snapshots + the RDS postgres log files
#                                                # covering the sampled window into <dir>
#   TARGET=aws ./db-sampler.sh settings          # one-shot: the settings that matter, as JSON
#
# One round trip per tick (a single SELECT returning one JSON object), so the
# sampler itself is ~0.1 % of the load it observes. Everything is a cumulative
# counter or a snapshot; the analysis (analyze-tail.py) takes deltas.
#
# What each field is for (PG16 names — pg_stat_checkpointer is PG17):
#   lsn              pg_current_wal_lsn: WAL bytes/s = the write rate the volume must absorb
#   act              client backends by (state, wait_event_type, wait_event, query prefix):
#                    what the 16-60 inserters are waiting ON when the tail spikes
#   bg               every non-client backend (checkpointer, autovacuum worker, walwriter,
#                    background writer) with its wait event: "checkpointer: DataFileSync"
#                    is a checkpoint's fsync phase; an autovacuum worker row IS an autovacuum
#   vac              pg_stat_progress_vacuum: which relation, which phase, how far
#   bgw              pg_stat_bgwriter: checkpoints_timed/req, checkpoint_write/sync_time,
#                    buffers_checkpoint/clean/backend, buffers_backend_fsync (backends doing
#                    the checkpointer's job), maxwritten_clean, buffers_alloc
#   wal              pg_stat_wal: wal_records, wal_fpi (full-page images — high right after
#                    a checkpoint), wal_bytes, wal_buffers_full, wal_write/sync counts+times
#   io               pg_stat_io (PG16): per backend_type x context: reads, writes, extends,
#                    fsyncs, evictions, hits, and their times. Evictions by client backends
#                    = shared_buffers pressure; fsyncs by checkpointer = checkpoint sync
#   tbl              pg_stat_user_tables for usage_events: n_tup_ins, n_ins_since_vacuum,
#                    last_autovacuum, autovacuum_count, last_autoanalyze; plus
#                    pg_statio (heap/idx blks read vs hit)
#   idx              per-index blocks read/hit + size (which index misses cache)
#   gin              pgstatginindex(properties GIN): pending_pages, pending_tuples — the
#                    fastupdate pending list; a sawtooth here IS the pending-list flush
#   locks            ungranted pg_locks by locktype+mode (relation extension, tuple, ...)
#   db               pg_stat_database: xact_commit, blks_read/hit, blk_read/write_time,
#                    temp_files/bytes, deadlocks
# Every 60 s (kind=size): table + per-index sizes.
set -euo pipefail

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
  ssh_() { ssh -o StrictHostKeyChecking=no -o LogLevel=ERROR -o ServerAliveInterval=20 -o ServerAliveCountMax=3 -i "$KEYFILE" "ec2-user@$APP_PUB" "$@"; }
  cmd="${1:-}"; tag="${2:-}"
  case "$cmd" in
    start|stop|settings)
      ssh_ "TARGET=local DATABASE_URL='$REMOTE_DSN' INTERVAL='${INTERVAL:-5}' bash -s -- $*" < "$0"; exit $? ;;
    fetch)
      dest="${3:?fetch <tag> <dir>}"; mkdir -p "$dest"
      ssh_ "cat /tmp/dbsample-$tag.jsonl" > "$dest/$tag.dbsample.jsonl" 2>/dev/null || true
      ssh_ "cat /tmp/dbsample-$tag.pgss-start.tsv 2>/dev/null" > "$dest/$tag.pgss-start.tsv" 2>/dev/null || true
      ssh_ "cat /tmp/dbsample-$tag.pgss-end.tsv 2>/dev/null"   > "$dest/$tag.pgss-end.tsv" 2>/dev/null || true
      ssh_ "cat /tmp/dbsample-$tag.window 2>/dev/null"          > "$dest/$tag.window" 2>/dev/null || true
      # RDS log files covering the window. RDS rotates hourly
      # (error/postgresql.log.YYYY-MM-DD-HH, UTC). The CLI paginates the download.
      # Every checkpoint (log_checkpoints), autovacuum (log_autovacuum_min_duration=0)
      # and lock wait > 1 s (log_lock_waits) is a line in here.
      if [ -s "$dest/$tag.window" ]; then
        read -r w_start w_end < "$dest/$tag.window"; : "${w_end:=$(date -u +%s)}"
        hours=$(python3 -c "import datetime as d,sys; a=int(sys.argv[1])-3600; b=int(sys.argv[2])+3600
print(' '.join(sorted({d.datetime.utcfromtimestamp(t).strftime('%Y-%m-%d-%H') for t in range(a,b+1,900)})))" "$w_start" "$w_end")
        : > "$dest/$tag.postgres.log"
        for f in $(aws_ rds describe-db-log-files --db-instance-identifier velox-bench-db \
                     --query 'DescribeDBLogFiles[].LogFileName' --output text); do
          fh=$(printf '%s' "$f" | sed -n 's/.*postgresql\.log\.\([0-9-]*\)$/\1/p'); [ -n "$fh" ] || continue
          case " $hours " in *" $fh "*) ;; *) continue ;; esac
          aws_ rds download-db-log-file-portion --db-instance-identifier velox-bench-db \
            --log-file-name "$f" --starting-token 0 --output text >> "$dest/$tag.postgres.log" 2>/dev/null \
            || echo "  (could not download $f)"
        done
        echo "  postgres log: $(wc -l < "$dest/$tag.postgres.log" | tr -d ' ') lines -> $dest/$tag.postgres.log"
      fi
      # Performance Insights (1 s wait-event load by wait event, and by SQL) and
      # Enhanced Monitoring (1 s OS disk await/util/queue) for the same window,
      # when the account grants them (pi:Get* inline policy; velox-bench-rds-monitoring
      # role). Both are best-effort; the sampler stands on its own without them.
      if [ -s "$dest/$tag.window" ]; then
        read -r w_start w_end < "$dest/$tag.window"; : "${w_end:=$(date -u +%s)}"
        RID=$(aws_ rds describe-db-instances --db-instance-identifier velox-bench-db --query 'DBInstances[0].DbiResourceId' --output text)
        s_iso=$(python3 -c "import datetime as d,sys; print(d.datetime.utcfromtimestamp(int(sys.argv[1])).strftime('%Y-%m-%dT%H:%M:%SZ'))" "$w_start")
        e_iso=$(python3 -c "import datetime as d,sys; print(d.datetime.utcfromtimestamp(int(sys.argv[1])).strftime('%Y-%m-%dT%H:%M:%SZ'))" "$w_end")
        # PI returns at most 600 data points per call, i.e. 10 minutes at 1 s
        # (a 1 h request silently comes back as its first 10 minutes — the
        # first control-series pull lost 90 % of the window that way). Page in
        # 600 s chunks for the 1 s waits; the 60 s by-SQL series can go hourly.
        : > "$dest/$tag.pi-waits.json"; : > "$dest/$tag.pi-sql.json"
        for ((t=w_start; t<w_end; t+=600)); do
          t2=$((t+600)); [ $t2 -gt $w_end ] && t2=$w_end
          a_iso=$(python3 -c "import datetime as d,sys; print(d.datetime.utcfromtimestamp(int(sys.argv[1])).strftime('%Y-%m-%dT%H:%M:%SZ'))" "$t")
          b_iso=$(python3 -c "import datetime as d,sys; print(d.datetime.utcfromtimestamp(int(sys.argv[1])).strftime('%Y-%m-%dT%H:%M:%SZ'))" "$t2")
          aws_ pi get-resource-metrics --service-type RDS --identifier "$RID" --start-time "$a_iso" --end-time "$b_iso" --period-in-seconds 1 \
            --metric-queries '[{"Metric":"db.load.avg","GroupBy":{"Group":"db.wait_event","Limit":10}}]' --output json >> "$dest/$tag.pi-waits.json" 2>/dev/null || true
        done
        for ((t=w_start; t<w_end; t+=3600)); do
          t2=$((t+3600)); [ $t2 -gt $w_end ] && t2=$w_end
          a_iso=$(python3 -c "import datetime as d,sys; print(d.datetime.utcfromtimestamp(int(sys.argv[1])).strftime('%Y-%m-%dT%H:%M:%SZ'))" "$t")
          b_iso=$(python3 -c "import datetime as d,sys; print(d.datetime.utcfromtimestamp(int(sys.argv[1])).strftime('%Y-%m-%dT%H:%M:%SZ'))" "$t2")
          aws_ pi get-resource-metrics --service-type RDS --identifier "$RID" --start-time "$a_iso" --end-time "$b_iso" --period-in-seconds 60 \
            --metric-queries '[{"Metric":"db.load.avg","GroupBy":{"Group":"db.sql_tokenized","Limit":10}}]' --output json >> "$dest/$tag.pi-sql.json" 2>/dev/null || true
        done
        [ -s "$dest/$tag.pi-waits.json" ] && echo "  performance insights: $dest/$tag.pi-waits.json (1 s waits), $tag.pi-sql.json (60 s by SQL)" || rm -f "$dest/$tag.pi-waits.json" "$dest/$tag.pi-sql.json"
        # Enhanced Monitoring lands in CloudWatch Logs group RDSOSMetrics, one JSON doc per second
        aws_ logs filter-log-events --log-group-name RDSOSMetrics --log-stream-names "$RID" \
          --start-time $((w_start*1000)) --end-time $((w_end*1000)) --output json --query 'events[].message' 2>/dev/null \
          | python3 -c "
import sys,json
try: msgs=json.load(sys.stdin)
except Exception: msgs=[]
out=open('$dest/$tag.em.jsonl','w'); n=0
for m in msgs:
    try: d=json.loads(m)
    except Exception: continue
    disk=[x for x in d.get('diskIO',[]) if x.get('device','').startswith('rdsdev')] or d.get('diskIO',[])
    out.write(json.dumps({'ts':d.get('timestamp'),'cpu':d.get('cpuUtilization',{}).get('total'),'wait':d.get('cpuUtilization',{}).get('wait'),
        'mem_dirty':d.get('memory',{}).get('dirty'),'mem_writeback':d.get('memory',{}).get('writeback'),'load1':d.get('loadAverageMinute',{}).get('one'),
        'disk':[{'dev':x.get('device'),'await':x.get('await'),'util':x.get('util'),'q':x.get('avgQueueLen'),'wkb':x.get('writeKbPS'),'rkb':x.get('readKbPS'),'tps':x.get('tps'),'reqsz':x.get('avgReqSz')} for x in disk]})+'\n'); n+=1
print(f'  enhanced monitoring: {n} one-second samples -> $dest/$tag.em.jsonl')
" || true
        [ -s "$dest/$tag.em.jsonl" ] || rm -f "$dest/$tag.em.jsonl"
      fi
      echo "  samples: $(wc -l < "$dest/$tag.dbsample.jsonl" | tr -d ' ') ticks -> $dest/$tag.dbsample.jsonl"
      exit 0 ;;
    *) echo "usage: $0 start|stop|fetch|settings <tag> [dir]"; exit 1 ;;
  esac
fi

: "${DATABASE_URL:?set DATABASE_URL}"
cmd="${1:-}"; tag="${2:-run}"
INTERVAL="${INTERVAL:-5}"
FILE="/tmp/dbsample-$tag.jsonl"

# The one statement per tick. Every sub-select is against a statistics view or
# a metapage read (pgstatginindex) — nothing scans a table.
TICK_SQL=$(cat <<'SQL'
SELECT json_build_object(
  'ts', extract(epoch from clock_timestamp())::bigint,
  'lsn', pg_current_wal_lsn()::text,
  'act', (SELECT coalesce(json_agg(json_build_object('st',state,'wt',wait_event_type,'we',wait_event,'q',left(regexp_replace(query,'\s+',' ','g'),50),'n',n)),'[]')
          FROM (SELECT state, wait_event_type, wait_event, query, count(*) n FROM pg_stat_activity
                WHERE backend_type='client backend' AND pid<>pg_backend_pid() AND datname=current_database()
                GROUP BY 1,2,3,4) s),
  'bg', (SELECT coalesce(json_agg(json_build_object('bt',backend_type,'wt',wait_event_type,'we',wait_event,'q',left(query,60))),'[]')
          FROM pg_stat_activity WHERE backend_type<>'client backend' AND backend_type IS NOT NULL),
  'ana', (SELECT coalesce(json_agg(json_build_object('rel',relid::regclass::text,'ph',phase,'tot',sample_blks_total,'scan',sample_blks_scanned)),'[]') FROM pg_stat_progress_analyze),
  'vac', (SELECT coalesce(json_agg(json_build_object('rel',relid::regclass::text,'ph',phase,'tot',heap_blks_total,'scan',heap_blks_scanned,'vacd',heap_blks_vacuumed,'ivc',index_vacuum_count,'dead',num_dead_tuples)),'[]') FROM pg_stat_progress_vacuum),
  'bgw', (SELECT row_to_json(b) FROM (SELECT checkpoints_timed ct, checkpoints_req cr, checkpoint_write_time cwt, checkpoint_sync_time cst, buffers_checkpoint bc, buffers_clean bcl, maxwritten_clean mwc, buffers_backend bb, buffers_backend_fsync bbf, buffers_alloc ba FROM pg_stat_bgwriter) b),
  'wal', (SELECT row_to_json(w) FROM (SELECT wal_records wr, wal_fpi fpi, wal_bytes wb, wal_buffers_full wbf, wal_write ww, wal_sync ws, wal_write_time wwt, wal_sync_time wst FROM pg_stat_wal) w),
  'io', (SELECT coalesce(json_agg(json_build_object('bt',backend_type,'ctx',context,'obj',object,'r',reads,'rt',read_time,'w',writes,'wt',write_time,'x',extends,'xt',extend_time,'f',fsyncs,'ft',fsync_time,'ev',evictions,'ru',reuses,'h',hits)),'[]')
          FROM pg_stat_io WHERE (reads>0 OR writes>0 OR extends>0 OR fsyncs>0 OR evictions>0 OR hits>0) AND object='relation'),
  'tbl', (SELECT row_to_json(t) FROM (SELECT s.n_tup_ins ins, s.n_live_tup live, s.n_dead_tup dead, s.n_ins_since_vacuum isv, s.n_mod_since_analyze msa,
                 extract(epoch from s.last_autovacuum)::bigint lav, extract(epoch from s.last_autoanalyze)::bigint laa, s.autovacuum_count avc, s.autoanalyze_count aac,
                 i.heap_blks_read hbr, i.heap_blks_hit hbh, i.idx_blks_read ibr, i.idx_blks_hit ibh, i.toast_blks_read tbr
          FROM pg_stat_user_tables s JOIN pg_statio_user_tables i USING (relid) WHERE s.relname='usage_events') t),
  'idx', (SELECT coalesce(json_agg(json_build_object('n',indexrelname,'r',idx_blks_read,'h',idx_blks_hit)),'[]') FROM pg_statio_user_indexes WHERE relname='usage_events'),
  'gin', (SELECT CASE WHEN EXISTS (SELECT 1 FROM pg_extension WHERE extname='pgstattuple') AND EXISTS (SELECT 1 FROM pg_class WHERE relname='idx_usage_events_properties_gin')
                 THEN (SELECT row_to_json(g) FROM (SELECT pending_pages pp, pending_tuples pt FROM pgstatginindex('idx_usage_events_properties_gin')) g) END),
  'locks', (SELECT coalesce(json_agg(json_build_object('lt',locktype,'m',mode,'g',granted,'rel',relation::regclass::text,'pg',page,'n',n)),'[]') FROM (SELECT locktype, mode, granted, relation, page, count(*) n FROM pg_locks WHERE locktype IN ('page','extend') OR NOT granted GROUP BY 1,2,3,4,5) l),
  'slow', (SELECT coalesce(json_agg(json_build_object('bt',backend_type,'wt',wait_event_type,'we',wait_event,'age',round(extract(epoch from clock_timestamp()-query_start)::numeric,3),'q',left(regexp_replace(query,'\s+',' ','g'),60))),'[]')
           FROM (SELECT * FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND state='active' AND backend_type='client backend' AND clock_timestamp()-query_start > interval '100 ms' ORDER BY query_start LIMIT 12) x),
  'db', (SELECT row_to_json(d) FROM (SELECT xact_commit xc, blks_read br, blks_hit bh, blk_read_time brt, blk_write_time bwt, temp_files tf, temp_bytes tb, deadlocks dl FROM pg_stat_database WHERE datname=current_database()) d)
)::text;
SQL
)
PGSS_SQL="SELECT calls, total_exec_time, mean_exec_time, rows, shared_blks_hit, shared_blks_read, shared_blks_dirtied, shared_blks_written, blk_read_time, blk_write_time, wal_records, wal_fpi, wal_bytes, left(regexp_replace(query,'\\s+',' ','g'),120) FROM pg_stat_statements ORDER BY total_exec_time DESC LIMIT 40"
SIZE_SQL="SELECT json_build_object('ts', extract(epoch from clock_timestamp())::bigint, 'kind','size', 'rows_est', (SELECT reltuples::bigint FROM pg_class WHERE relname='usage_events'), 'heap', pg_table_size('usage_events'), 'idx', (SELECT json_object_agg(indexrelname, pg_relation_size(indexrelid)) FROM pg_stat_user_indexes WHERE relname='usage_events'), 'frozen_age', (SELECT age(relfrozenxid) FROM pg_class WHERE relname='usage_events'), 'toast_ins', (SELECT n_tup_ins FROM pg_stat_all_tables WHERE relid=(SELECT reltoastrelid FROM pg_class WHERE relname='usage_events')), 'wal_lsn', pg_current_wal_lsn()::text)::text;"

case "$cmd" in
  settings)
    psql "$DATABASE_URL" -qtA -c "SELECT json_build_object('indexes', (SELECT json_object_agg(c.relname, coalesce(array_to_string(c.reloptions,','),'')) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid='usage_events'::regclass), 'table_reloptions', (SELECT coalesce(array_to_string(reloptions,','),'') FROM pg_class WHERE relname='usage_events'))"
    psql "$DATABASE_URL" -qtA -c "SELECT json_object_agg(name, setting) FROM pg_settings WHERE name IN ('wal_segment_size','data_checksums','deadlock_timeout','log_min_duration_statement','wal_writer_delay','wal_writer_flush_after','checkpoint_flush_after','bgwriter_flush_after','backend_flush_after','shared_buffers','effective_cache_size','max_wal_size','min_wal_size','checkpoint_timeout','checkpoint_completion_target','wal_buffers','wal_compression','full_page_writes','synchronous_commit','bgwriter_delay','bgwriter_lru_maxpages','autovacuum_naptime','autovacuum_vacuum_insert_threshold','autovacuum_vacuum_insert_scale_factor','autovacuum_vacuum_scale_factor','autovacuum_analyze_scale_factor','autovacuum_vacuum_cost_delay','autovacuum_vacuum_cost_limit','autovacuum_max_workers','autovacuum_work_mem','maintenance_work_mem','vacuum_cost_page_hit','vacuum_cost_page_miss','vacuum_cost_page_dirty','gin_pending_list_limit','log_autovacuum_min_duration','log_checkpoints','log_lock_waits','track_io_timing','track_wal_io_timing','shared_preload_libraries','rds.adaptive_autovacuum','server_version','huge_pages','io_combine_limit','random_page_cost','effective_io_concurrency','max_connections')" ;;
  start)
    psql "$DATABASE_URL" -qtA -c "CREATE EXTENSION IF NOT EXISTS pgstattuple" >/dev/null 2>&1 || true
    psql "$DATABASE_URL" -qtA -c "CREATE EXTENSION IF NOT EXISTS pg_stat_statements" >/dev/null 2>&1 || true
    psql "$DATABASE_URL" -qtA -c "SELECT pg_stat_statements_reset()" >/dev/null 2>&1 || true
    psql "$DATABASE_URL" -qtA -F $'\t' -c "$PGSS_SQL" > "/tmp/dbsample-$tag.pgss-start.tsv" 2>/dev/null || true
    # pgstatginindex needs superuser or pg_stat_scan_tables; probe once and drop
    # the field if RDS does not grant it, so one denied function cannot void
    # every tick.
    if psql "$DATABASE_URL" -qtA -c "SELECT pending_pages FROM pgstatginindex('idx_usage_events_properties_gin')" >/dev/null 2>&1; then
      printf '%s\n' "$TICK_SQL" > "/tmp/dbsample-$tag.sql"
    else
      echo "  (pgstatginindex not permitted or index absent — no GIN pending-list samples)"
      printf '%s\n' "$TICK_SQL" | sed "s/'gin', (SELECT CASE WHEN EXISTS/'gin', (SELECT NULL WHERE false AND EXISTS/" > "/tmp/dbsample-$tag.sql"
    fi
    printf '%s\n' "$SIZE_SQL" > "/tmp/dbsample-$tag.size.sql"
    date -u +%s > "/tmp/dbsample-$tag.window"
    : > "$FILE"
    # One psql per tick keeps a failure isolated (a dropped connection costs one
    # sample, not the series). Size snapshot every 12 ticks.
    nohup bash -c "n=0; while true; do
        psql '$DATABASE_URL' -qtAX -f '/tmp/dbsample-$tag.sql' >> '$FILE' 2>/dev/null || echo '{\"ts\":'\$(date -u +%s)',\"err\":\"tick failed\"}' >> '$FILE'
        n=\$((n+1)); if [ \$((n % 12)) -eq 0 ]; then psql '$DATABASE_URL' -qtAX -f '/tmp/dbsample-$tag.size.sql' >> '$FILE' 2>/dev/null || true; fi
        sleep $INTERVAL; done" > /dev/null 2>&1 &
    echo $! > "/tmp/dbsample-$tag.pid"
    sleep 3
    if [ -s "$FILE" ] && head -c 400 "$FILE" | grep -q '"lsn"'; then
      echo "  sampler running (pid $(cat /tmp/dbsample-$tag.pid)), first tick ok: $(head -c 120 "$FILE")..."
    else
      echo "  FATAL: sampler produced no valid tick:"; tail -2 "$FILE"; psql "$DATABASE_URL" -qtAX -f "/tmp/dbsample-$tag.sql" 2>&1 | tail -3; exit 1
    fi ;;
  stop)
    [ -f "/tmp/dbsample-$tag.pid" ] && kill "$(cat /tmp/dbsample-$tag.pid)" 2>/dev/null; pkill -f "dbsample-$tag.jsonl" 2>/dev/null || true
    printf '%s %s\n' "$(cat /tmp/dbsample-$tag.window 2>/dev/null || echo 0)" "$(date -u +%s)" > "/tmp/dbsample-$tag.window"
    psql "$DATABASE_URL" -qtA -F $'\t' -c "$PGSS_SQL" > "/tmp/dbsample-$tag.pgss-end.tsv" 2>/dev/null || true
    echo "  sampler stopped: $(wc -l < "$FILE" | tr -d ' ') ticks in $FILE" ;;
  *) echo "usage: $0 start|stop|settings <tag>"; exit 1 ;;
esac
