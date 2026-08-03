#!/usr/bin/env python3
"""Local webhook receiver for testing Velox's outbound deliveries.

Runs a bare HTTP server (default 127.0.0.1:9099) and appends one JSON line
per delivery to the log file: UTC time, path, the Velox-Signature header,
and the body. Register endpoints against it with any path:

    http://localhost:9099/ok      -> 200 (any path not starting with /fail)
    http://localhost:9099/fail... -> 500 (drive the retry ladder / failure UX)

Verifying a capture's signature against the endpoint's whsec_ secret:

    t, v1 = dict(kv.split("=", 1) for kv in rec["sig"].split(","))...
    hmac.new(secret, f"{t}.{body}".encode(), sha256).hexdigest() == v1

Used by the FLOW W walk (2026-08-03) — the /fail path drove the full
1m->5m->30m->2h->24h retry ladder, and the captured headers verified both
single-secret and dual-signing (rotation grace) deliveries offline.

Usage: python3 scripts/dev/webhook-sink.py [logfile] [port]
"""
import http.server
import json
import sys
import time

LOG = sys.argv[1] if len(sys.argv) > 1 else "webhook-sink.jsonl"
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 9099


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        rec = {
            "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "path": self.path,
            "sig": self.headers.get("Velox-Signature", ""),
            "body": body.decode(errors="replace"),
        }
        with open(LOG, "a") as f:
            f.write(json.dumps(rec) + "\n")
        self.send_response(500 if self.path.startswith("/fail") else 200)
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *_):  # keep stdout quiet; the JSONL is the record
        pass


if __name__ == "__main__":
    print(f"webhook sink on 127.0.0.1:{PORT} -> {LOG} (paths under /fail return 500)")
    http.server.HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
