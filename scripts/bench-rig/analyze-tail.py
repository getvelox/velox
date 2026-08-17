#!/usr/bin/env python3
"""Attribute latency-tail windows in a benchmark series to what the database was doing.

    ./analyze-tail.py <results-dir> [--series TAG] [--spike-factor 3] [--min-buckets 2]

Inputs (all optional except the k6 samples):
  <run>.samples.jsonl.gz     k6 raw samples per run (measure.sh CAPTURE=1)
  <TAG>.dbsample.jsonl       db-sampler.sh ticks (5 s), one JSON object per line
  <TAG>.postgres.log         RDS postgres log for the window (db-sampler.sh fetch)
  <run>.rds.*.json           CloudWatch series per run (measure.sh)

What it does:
  1. Buckets the {scenario:offered} http_req_duration samples of every run into
     10 s buckets: n, p50, p99, max, dropped_iterations.
  2. A "tail window" is >= --min-buckets consecutive buckets whose p99 exceeds
     --spike-factor x the run's median bucket p99 (default 3x). p50 is reported
     alongside so "tail-only" vs "everything slowed" is visible.
  3. For each window it prints, from the sampler ticks: the per-tick deltas of
     the DB counters INSIDE the window versus a baseline (the 5 minutes before),
     the client-backend wait events that appeared, autovacuum progress rows,
     checkpointer/bgwriter/autovacuum-worker wait events, GIN pending list,
     ungranted locks, and the postgres log lines from 3 min before to 1 min after.
Nothing here decides the cause; it lays the evidence side by side so a human can.
"""
import glob, gzip, json, os, re, statistics, sys
from collections import defaultdict

def parse_args(argv):
    a = {"dir": None, "series": None, "factor": 3.0, "minb": 2}
    it = iter(argv)
    for x in it:
        if x == "--series": a["series"] = next(it)
        elif x == "--spike-factor": a["factor"] = float(next(it))
        elif x == "--min-buckets": a["minb"] = int(next(it))
        else: a["dir"] = x
    if not a["dir"]: sys.exit(__doc__)
    return a

def pct(v, p):
    if not v: return None
    v = sorted(v); return v[min(len(v)-1, int(len(v)*p))]

_epoch_cache = {}

def load_k6(path):
    """-> dict bucket_epoch10 -> {'n','p50','p99','max','drops'} for scenario=offered."""
    buckets = defaultdict(list); drops = defaultdict(float)
    with gzip.open(path, "rt") as f:
        for line in f:
            # cheap prefilter: 700k+ lines per 10-minute run, json.loads dominates
            if '"http_req_duration"' not in line and '"dropped_iterations"' not in line: continue
            try: o = json.loads(line)
            except Exception: continue
            if o.get("type") != "Point": continue
            m = o.get("metric"); d = o["data"]
            ts = d["time"][:19]  # "YYYY-MM-DDTHH:MM:SS"; k6 writes 9-digit fractions
            t = _epoch_cache.get(ts)
            if t is None:
                import calendar, time as _t
                try: t = calendar.timegm(_t.strptime(ts, "%Y-%m-%dT%H:%M:%S"))
                except Exception: continue
                _epoch_cache[ts] = t
            b = int(t // 10) * 10
            if m == "dropped_iterations": drops[b] += d["value"]; continue
            if m != "http_req_duration": continue
            if d.get("tags", {}).get("scenario") != "offered": continue
            buckets[b].append(d["value"])
    out = {}
    for b in sorted(set(buckets) | set(drops)):
        v = buckets.get(b, [])
        out[b] = {"n": len(v), "p50": pct(v, .5), "p99": pct(v, .99), "max": max(v) if v else None, "drops": drops.get(b, 0)}
    return out

def find_windows(bk, factor, minb):
    p99s = [x["p99"] for x in bk.values() if x["p99"] is not None and x["n"] >= 50]
    if not p99s: return [], None
    med = statistics.median(p99s)
    keys = sorted(bk); wins = []; cur = []
    for k in keys:
        x = bk[k]
        hot = x["p99"] is not None and x["n"] >= 50 and x["p99"] > factor * med
        if hot: cur.append(k)
        else:
            if len(cur) >= minb: wins.append((cur[0], cur[-1] + 10))
            cur = []
    if len(cur) >= minb: wins.append((cur[0], cur[-1] + 10))
    return wins, med

def load_ticks(path):
    ticks = []
    for line in open(path):
        line = line.strip()
        if not line: continue
        try: o = json.loads(line)
        except Exception: continue
        if "lsn" in o: ticks.append(o)
    return ticks

def load_em(path):
    """Enhanced Monitoring 1 s samples -> list of (epoch, await_ms, queue, write_kBps, read_kBps, tps, cpu, wait)."""
    out = []
    import calendar, time as _t
    for line in open(path):
        try: o = json.loads(line)
        except Exception: continue
        try: ts = calendar.timegm(_t.strptime(o["ts"][:19], "%Y-%m-%dT%H:%M:%S"))
        except Exception: continue
        d = (o.get("disk") or [{}])[0]
        out.append((ts, d.get("await"), d.get("q"), d.get("wkb"), d.get("rkb"), d.get("tps"), o.get("cpu"), o.get("wait")))
    return sorted(out)

def load_pi(path):
    """PI 1 s db.load.avg by wait event -> dict epoch -> {wait_event: load}. File may hold several concatenated JSON docs."""
    import calendar, time as _t, datetime as dt
    txt = open(path).read()
    docs = []; dec = json.JSONDecoder(); i = 0
    while i < len(txt):
        while i < len(txt) and txt[i].isspace(): i += 1
        if i >= len(txt): break
        try: o, j = dec.raw_decode(txt, i)
        except Exception: break
        docs.append(o); i = j
    series = defaultdict(dict)
    for d in docs:
        for m in d.get("MetricList", []):
            dims = m["Key"].get("Dimensions")
            name = dims["db.wait_event.name"] if dims else "TOTAL"
            for p in m.get("DataPoints", []):
                v = p.get("Value")
                if v is None: continue
                try:
                    t = dt.datetime.fromisoformat(p["Timestamp"]).timestamp()
                except Exception: continue
                series[int(t)][name] = v
    return series

def em_summary(em, a, b):
    sel = [x for x in em if a <= x[0] < b]
    if not sel: return None
    def col(i):
        v = sorted(x[i] for x in sel if x[i] is not None)
        return (v[len(v)//2], v[-1]) if v else (None, None)
    return {"n": len(sel), "await_ms(med/max)": col(1), "queue(med/max)": col(2), "write_MBps(med/max)": tuple(round(x/1024,1) if x else x for x in col(3)), "read_MBps(med/max)": tuple(round(x/1024,1) if x else x for x in col(4)), "tps(med/max)": col(5), "cpu%(med/max)": col(6), "iowait%(med/max)": col(7)}

def pi_summary(pi, a, b):
    tot = defaultdict(float); n = 0
    for t in range(int(a), int(b)):
        row = pi.get(t)
        if not row: continue
        n += 1
        for k, v in row.items(): tot[k] += v
    if not n: return None
    return {k: round(v/n, 2) for k, v in sorted(tot.items(), key=lambda kv: -kv[1]) if k != "TOTAL"}, round(tot.get("TOTAL", 0)/n, 2), n

def lsn_bytes(s):
    hi, lo = s.split("/"); return (int(hi, 16) << 32) + int(lo, 16)

def io_key(r): return (r["bt"], r["ctx"])

def tick_deltas(t0, t1):
    """counter deltas between two ticks, normalised per second."""
    dt = max(1, t1["ts"] - t0["ts"])
    d = {}
    d["wal_MBps"] = (lsn_bytes(t1["lsn"]) - lsn_bytes(t0["lsn"])) / dt / 1e6
    b0, b1 = t0["bgw"], t1["bgw"]
    d["ckpt_timed"] = b1["ct"] - b0["ct"]; d["ckpt_req"] = b1["cr"] - b0["cr"]
    d["ckpt_sync_ms"] = b1["cst"] - b0["cst"]; d["ckpt_write_ms"] = b1["cwt"] - b0["cwt"]
    d["buf_ckpt/s"] = (b1["bc"] - b0["bc"]) / dt; d["buf_backend/s"] = (b1["bb"] - b0["bb"]) / dt
    d["backend_fsync"] = b1["bbf"] - b0["bbf"]; d["buf_alloc/s"] = (b1["ba"] - b0["ba"]) / dt
    w0, w1 = t0["wal"], t1["wal"]
    d["wal_fpi/s"] = (w1["fpi"] - w0["fpi"]) / dt; d["wal_buffers_full/s"] = (w1["wbf"] - w0["wbf"]) / dt
    d["wal_sync/s"] = (w1["ws"] - w0["ws"]) / dt; d["wal_sync_ms/s"] = (w1["wst"] - w0["wst"]) / dt
    tb0, tb1 = t0.get("tbl") or {}, t1.get("tbl") or {}
    if tb0 and tb1:
        d["rows_ins/s"] = (tb1["ins"] - tb0["ins"]) / dt
        d["heap_read/s"] = (tb1["hbr"] - tb0["hbr"]) / dt; d["idx_read/s"] = (tb1["ibr"] - tb0["ibr"]) / dt
        d["autovac_count"] = tb1["avc"] - tb0["avc"]; d["autoanalyze_count"] = tb1["aac"] - tb0["aac"]
    db0, db1 = t0.get("db") or {}, t1.get("db") or {}
    if db0 and db1:
        d["xact/s"] = (db1["xc"] - db0["xc"]) / dt; d["blks_read/s"] = (db1["br"] - db0["br"]) / dt
        d["blk_read_ms/s"] = (db1["brt"] - db0["brt"]) / dt; d["blk_write_ms/s"] = (db1["bwt"] - db0["bwt"]) / dt
    io0 = {io_key(r): r for r in t0.get("io", [])}; io1 = {io_key(r): r for r in t1.get("io", [])}
    for k, r1 in io1.items():
        r0 = io0.get(k)
        if not r0: continue
        for f, name in (("r", "reads"), ("w", "writes"), ("x", "extends"), ("f", "fsyncs"), ("ev", "evictions")):
            v = (r1.get(f) or 0) - (r0.get(f) or 0)
            if v: d[f"io.{k[0]}.{k[1]}.{name}/s"] = v / dt
        for f, name in (("rt", "read_ms"), ("wt", "write_ms"), ("ft", "fsync_ms"), ("xt", "extend_ms")):
            v = (r1.get(f) or 0) - (r0.get(f) or 0)
            if v: d[f"io.{k[0]}.{k[1]}.{name}/s"] = v / dt
    ix0 = {r["n"]: r for r in t0.get("idx", [])}; ix1 = {r["n"]: r for r in t1.get("idx", [])}
    for n, r1 in ix1.items():
        r0 = ix0.get(n)
        if r0 and (r1["r"] - r0["r"]): d[f"idxread.{n}/s"] = (r1["r"] - r0["r"]) / dt
    return d

def summarize_range(ticks, a, b):
    """aggregate deltas over ticks in [a,b): sum of per-tick deltas / span; plus wait-event census."""
    sel = [t for t in ticks if a <= t["ts"] < b]
    if len(sel) < 2: return None, {}, [], [], [], []
    tot = tick_deltas(sel[0], sel[-1])
    waits = defaultdict(int); bgw = defaultdict(int); vac = []; gin = []; locks = defaultdict(int)
    for t in sel:
        for r in t.get("act", []):
            if r.get("st") == "idle": continue
            key = f'{r.get("wt")}:{r.get("we")}' if r.get("wt") else "CPU/running"
            waits[key] += r["n"]
        for r in t.get("bg", []):
            if r.get("we") and not str(r["we"]).endswith("Main"): bgw[f'{r["bt"]}:{r["wt"]}:{r["we"]}'] += 1
            if r["bt"] == "autovacuum worker": bgw[f'autovacuum worker RUNNING ({(r.get("q") or "")[:40]})'] += 1
        for r in t.get("vac", []): vac.append((t["ts"], r))
        g = t.get("gin")
        if g: gin.append((t["ts"], g.get("pp"), g.get("pt")))
        for r in t.get("locks", []): locks[f'{r["lt"]}:{r["m"]}'] += r["n"]
    return tot, dict(waits), dict(bgw), vac, gin, dict(locks)

def fmt(v):
    if v is None: return "-"
    if isinstance(v, float): return f"{v:,.1f}" if abs(v) >= 10 else f"{v:,.2f}"
    return str(v)

def main():
    a = parse_args(sys.argv[1:])
    d = a["dir"]
    runs = sorted(glob.glob(os.path.join(d, "*.samples.jsonl.gz")))
    if not runs: sys.exit(f"no *.samples.jsonl.gz in {d}")
    tickfiles = glob.glob(os.path.join(d, f"{a['series'] or '*'}.dbsample.jsonl"))
    ticks = []
    for tf in tickfiles: ticks += load_ticks(tf)
    ticks.sort(key=lambda t: t["ts"])
    logs = []
    for lf in glob.glob(os.path.join(d, f"{a['series'] or '*'}.postgres.log")):
        for line in open(lf, errors="replace"):
            m = re.match(r"(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) UTC", line)
            if not m:
                # continuation line (autovacuum reports span ~10 tab-indented lines): fold into the previous entry
                if logs and line.startswith(("\t", " ")): logs[-1] = (logs[-1][0], logs[-1][1] + " | " + line.strip())
                continue
            import datetime as dt
            ts = dt.datetime.strptime(m.group(1), "%Y-%m-%d %H:%M:%S").replace(tzinfo=dt.timezone.utc).timestamp()
            logs.append((ts, line.rstrip()))
    logs.sort()
    em = []; pi = {}
    for f in glob.glob(os.path.join(d, f"{a['series'] or '*'}.em.jsonl")): em += load_em(f)
    em.sort()
    for f in glob.glob(os.path.join(d, f"{a['series'] or '*'}.pi-waits.json")): pi.update(load_pi(f))
    print(f"series: {len(runs)} runs, {len(ticks)} db ticks, {len(logs)} postgres log lines, {len(em)} EM seconds, {len(pi)} PI seconds")
    if ticks:
        print(f"db ticks span {ticks[0]['ts']} .. {ticks[-1]['ts']} ({(ticks[-1]['ts']-ticks[0]['ts'])/60:.0f} min)")
    import datetime as dt
    def hms(ts): return dt.datetime.utcfromtimestamp(ts).strftime("%H:%M:%S")

    for run in runs:
        tag = os.path.basename(run).replace(".samples.jsonl.gz", "")
        bk = load_k6(run)
        wins, med = find_windows(bk, a["factor"], a["minb"])
        if not bk: print(f"\n## {tag}: no offered samples"); continue
        keys = sorted(bk); span = (keys[0], keys[-1] + 10)
        print(f"\n## {tag}  {hms(span[0])}–{hms(span[1])}  median bucket p99 {fmt(med)} ms  tail windows (>{a['factor']}x, >={a['minb']} buckets): {len(wins)}")
        # whole-run DB summary for context
        if ticks:
            tot, waits, bgw, vac, gin, locks = summarize_range(ticks, span[0], span[1])
            if tot:
                print("   run avg: " + "  ".join(f"{k}={fmt(tot[k])}" for k in ("wal_MBps", "rows_ins/s", "ckpt_timed", "ckpt_req", "buf_backend/s", "backend_fsync", "wal_fpi/s", "blks_read/s") if k in tot))
                topw = sorted(waits.items(), key=lambda kv: -kv[1])[:6]
                print("   run waits (non-idle client backends, tick-samples): " + ", ".join(f"{k}={v}" for k, v in topw))
        for (w0, w1) in wins:
            print(f"\n   ### TAIL WINDOW {hms(w0)}–{hms(w1)} ({(w1-w0)} s)")
            for k in range(w0, w1, 10):
                x = bk.get(k)
                if x: print(f"      {hms(k)} n={x['n']:5d} p50={fmt(x['p50'])} p99={fmt(x['p99'])} max={fmt(x['max'])} drops={int(x['drops'])}")
            if not ticks: print("      (no db samples for this series)"); continue
            base = summarize_range(ticks, w0 - 300, w0 - 30)
            win = summarize_range(ticks, w0 - 10, w1 + 10)
            if not base[0] or not win[0]: print("      (db samples do not cover this window)"); continue
            bt, wt = base[0], win[0]
            print("      counter                       baseline(5m before)   window       ratio")
            for k in sorted(set(bt) | set(wt)):
                b, w = bt.get(k, 0), wt.get(k, 0)
                if (abs(b) < 1e-9 and abs(w) < 1e-9): continue
                ratio = (w / b) if b else float("inf")
                flag = "  <<<" if (b and (ratio > 3 or ratio < 0.33)) or (not b and w) else ""
                print(f"      {k:30s} {fmt(b):>18s}   {fmt(w):>10s}   {('inf' if ratio==float('inf') else f'{ratio:.1f}'):>6s}{flag}")
            print("      client waits (tick-samples) baseline -> window:")
            for k in sorted(set(base[1]) | set(win[1]), key=lambda k: -(win[1].get(k, 0))):
                print(f"         {k:40s} {base[1].get(k,0):5d} -> {win[1].get(k,0):5d}")
            if win[2]: print("      background backends seen in window: " + ", ".join(f"{k}x{v}" for k, v in sorted(win[2].items(), key=lambda kv: -kv[1])[:8]))
            if base[2]: print("      background backends in baseline:    " + ", ".join(f"{k}x{v}" for k, v in sorted(base[2].items(), key=lambda kv: -kv[1])[:8]))
            if win[3]:
                print("      autovacuum progress in window:")
                for ts, r in win[3][:: max(1, len(win[3]) // 8)]: print(f"         {hms(ts)} {r}")
            if win[4]:
                pp = [g[1] for g in win[4] if g[1] is not None]
                bpp = [g[1] for g in base[4] if g[1] is not None]
                print(f"      GIN pending pages: baseline min/max {min(bpp) if bpp else '-'}/{max(bpp) if bpp else '-'}  window min/max {min(pp) if pp else '-'}/{max(pp) if pp else '-'}")
            if win[5]: print(f"      ungranted locks in window: {win[5]}")
            if em:
                eb, ew = em_summary(em, w0 - 300, w0 - 30), em_summary(em, w0 - 5, w1 + 5)
                if eb and ew:
                    print("      enhanced monitoring (1 s disk): baseline -> window")
                    for k in ("await_ms(med/max)", "queue(med/max)", "write_MBps(med/max)", "read_MBps(med/max)", "tps(med/max)", "cpu%(med/max)", "iowait%(med/max)"):
                        print(f"         {k:22s} {str(eb[k]):>18s} -> {str(ew[k])}")
                    # the seconds inside the window with the worst await
                    worst = sorted([x for x in em if w0 - 5 <= x[0] < w1 + 5 and x[1] is not None], key=lambda x: -x[1])[:5]
                    print("         worst seconds (await ms, queue, write MB/s, tps): " + ", ".join(f"{hms(x[0])}:{x[1]}/{x[2]}/{round((x[3] or 0)/1024,1)}/{x[5]}" for x in worst))
            if pi:
                pb, pw = pi_summary(pi, w0 - 300, w0 - 30), pi_summary(pi, w0 - 5, w1 + 5)
                if pb and pw:
                    print(f"      performance insights (avg active sessions): baseline total {pb[1]} -> window total {pw[1]}")
                    keys = list(pw[0].keys())[:8]
                    for k in keys: print(f"         {k:34s} {pb[0].get(k,0):6.2f} -> {pw[0].get(k,0):6.2f}")
            rel = [(ts, l) for ts, l in logs if w0 - 180 <= ts <= w1 + 60]
            if rel:
                print(f"      postgres log {hms(w0-180)}–{hms(w1+60)} ({len(rel)} lines; checkpoint/vacuum/lock/etc):")
                for ts, l in rel[:60]:
                    print("         " + l[:600])
            elif logs: print("      postgres log: no lines in range")
    # series-wide log census
    if logs:
        ck = [l for _, l in logs if "checkpoint complete" in l]
        av = [l for _, l in logs if "automatic vacuum" in l or "automatic analyze" in l]
        lw = [l for _, l in logs if "still waiting for" in l or "acquired" in l]
        print(f"\nlog census: {len(ck)} checkpoints complete, {len(av)} autovacuum/analyze, {len(lw)} lock-wait lines")
        for l in ck[-3:]: print("   " + l[:230])
        for l in av[-6:]: print("   " + l[:700])

if __name__ == "__main__":
    main()
