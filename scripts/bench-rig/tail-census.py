#!/usr/bin/env python3
"""Series-wide census of the tail mechanism, for comparing a treatment to its control.

    ./tail-census.py <results-dir> --series TAG [--series TAG2 ...]

Per series (only the measured window: from the first k6 sample to the last):
  k6:   10 s buckets with p99 > 3x series median; > 5x; worst bucket p99; drops
  PI:   seconds with LWLock:WALWrite >= 8 sessions (a segment-init convoy);
        seconds with any IO:WALInitSync / IO:WALInitWrite; total AAS median
  EM:   seconds with device write >= 110 MB/s (the gp3 <400 GB cap is 125 MiB/s);
        seconds with device-mapper queue >= 30; median/max write MB/s
  DB:   sampler ticks with a client backend on WALInit*; wal_buffers_full delta;
        checkpoints (timed / requested), WAL MB/s
  log:  checkpoints complete, "WAL file(s) added" total, autovacuum count
"""
import glob, gzip, json, os, re, statistics, sys, calendar, time
from collections import defaultdict
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from importlib.machinery import SourceFileLoader
at = SourceFileLoader("analyze_tail", os.path.join(os.path.dirname(os.path.abspath(__file__)), "analyze-tail.py")).load_module()

def census(d, tag):
    runs = sorted(glob.glob(os.path.join(d, f"{tag}-run*.samples.jsonl.gz")))
    if not runs: return None
    bk = {}
    for r in runs: bk.update(at.load_k6(r))
    keys = sorted(k for k, v in bk.items() if v["n"] >= 50)
    t0, t1 = keys[0], keys[-1] + 10
    p99s = [bk[k]["p99"] for k in keys]; med = statistics.median(p99s)
    out = {"window": (t0, t1), "buckets": len(keys), "median_p99": round(med, 1),
           "buckets>3x": sum(1 for k in keys if bk[k]["p99"] > 3 * med), "buckets>5x": sum(1 for k in keys if bk[k]["p99"] > 5 * med),
           "worst_p99": round(max(p99s), 1), "drops": int(sum(bk[k]["drops"] for k in keys))}
    pi = {}
    for f in glob.glob(os.path.join(d, f"{tag}.pi-waits.json")): pi.update(at.load_pi(f))
    if pi:
        secs = [pi[t] for t in range(t0, t1) if t in pi]
        out["pi_seconds"] = len(secs)
        out["pi_walwrite>=8"] = sum(1 for r in secs if r.get("LWLock:WALWrite", 0) >= 8)
        out["pi_walinit_secs"] = sum(1 for r in secs if r.get("IO:WALInitSync", 0) > 0 or r.get("IO:WALInitWrite", 0) > 0)
        tot = [r.get("TOTAL", 0) for r in secs]
        out["pi_aas_med/max"] = (round(statistics.median(tot), 1), round(max(tot), 1)) if tot else None
    em = []
    for f in glob.glob(os.path.join(d, f"{tag}.em.jsonl")): em += at.load_em(f)
    em = [x for x in em if t0 <= x[0] < t1]
    if em:
        w = [x[3] / 1024 for x in em if x[3] is not None]; q = [x[2] for x in em if x[2] is not None]
        out["em_seconds"] = len(em); out["em_write>=110MBps"] = sum(1 for v in w if v >= 110); out["em_queue>=30"] = sum(1 for v in q if v >= 30)
        out["em_write_med/max"] = (round(statistics.median(w), 1), round(max(w), 1)); out["em_queue_med/max"] = (round(statistics.median(q), 1), round(max(q), 1))
    ticks = []
    for f in glob.glob(os.path.join(d, f"{tag}.dbsample.jsonl")): ticks += at.load_ticks(f)
    ticks = [t for t in ticks if t0 <= t["ts"] < t1]
    if len(ticks) > 2:
        out["db_ticks"] = len(ticks)
        out["db_walinit_ticks"] = sum(1 for t in ticks for a in t.get("act", []) if a.get("we") in ("WALInitSync", "WALInitWrite"))
        out["db_walwrite>=8_ticks"] = sum(1 for t in ticks for a in t.get("act", []) if a.get("we") == "WALWrite" and a.get("n", 0) >= 8)
        d0, d1 = ticks[0], ticks[-1]
        out["wal_buffers_full"] = d1["wal"]["wbf"] - d0["wal"]["wbf"]
        out["ckpt_timed/req"] = (d1["bgw"]["ct"] - d0["bgw"]["ct"], d1["bgw"]["cr"] - d0["bgw"]["cr"])
        out["wal_MBps"] = round((at.lsn_bytes(d1["lsn"]) - at.lsn_bytes(d0["lsn"])) / max(1, d1["ts"] - d0["ts"]) / 1e6, 1)
        out["ckpt_sync_ms_total"] = d1["bgw"]["cst"] - d0["bgw"]["cst"]
    logs = []
    for lf in glob.glob(os.path.join(d, f"{tag}.postgres.log")):
        for line in open(lf, errors="replace"):
            m = re.match(r"(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) UTC", line)
            if not m: continue
            ts = calendar.timegm(time.strptime(m.group(1), "%Y-%m-%d %H:%M:%S"))
            if t0 - 60 <= ts <= t1 + 60: logs.append(line)
    if logs:
        ck = [l for l in logs if "checkpoint complete" in l]
        added = sum(int(m.group(1)) for l in ck for m in [re.search(r"(\d+) WAL file\(s\) added", l)] if m)
        recycled = [int(m.group(1)) for l in ck for m in [re.search(r"(\d+) recycled", l)] if m]
        syncs = [float(m.group(1)) for l in ck for m in [re.search(r"sync=([\d.]+) s", l)] if m]
        out["log_checkpoints"] = len(ck); out["log_wal_added"] = added; out["log_recycled_med"] = statistics.median(recycled) if recycled else None
        out["log_ckpt_sync_max_s"] = max(syncs) if syncs else None
        out["log_autovacuum"] = sum(1 for l in logs if "automatic vacuum of table" in l and "usage_events" in l)
        out["log_slow_stmts"] = sum(1 for l in logs if "duration:" in l)
    return out

def main():
    d = None; tags = []
    it = iter(sys.argv[1:])
    for x in it:
        if x == "--series": tags.append(next(it))
        else: d = x
    if not d or not tags: sys.exit(__doc__)
    rows = {t: census(d if not os.path.isdir(os.path.join(d, f"tail-{t}")) else os.path.join(d, f"tail-{t}"), t) for t in tags}
    keys = []
    for r in rows.values():
        if r:
            for k in r:
                if k not in keys: keys.append(k)
    print(f"{'metric':26s}" + "".join(f"{t:>28s}" for t in tags))
    for k in keys:
        if k == "window": continue
        print(f"{k:26s}" + "".join(f"{str(rows[t].get(k, '-') if rows[t] else '-'):>28s}" for t in tags))

if __name__ == "__main__":
    main()
