//go:build ignore

// A stub ingest endpoint with a KNOWN fixed service time and an exact
// server-side event counter, used to calibrate the load generator.
//
// Build-tagged `ignore` so it stays out of ./... — run it via calibrate.sh,
// which is the only supported entry point.
//
// The point: ingest.js reports numbers about Velox, and nothing about Velox
// tells us whether those numbers are true. Pointing the same script at a
// system whose behaviour we set exactly — 20ms per request, n events counted
// server-side — makes the load generator itself falsifiable.
//
// DELAY_MS      fixed service time per request (the "truth" to recover)
// FAIL_PCT      percentage of requests answered 500, to prove failures are
//
//	reported and NOT counted as delivered throughput
//
// MAX_INFLIGHT  concurrent request ceiling; beyond it the stub answers 503
// TAIL_PCT/TAIL_MS  a known bimodal tail (TAIL_PCT% of requests take TAIL_MS),
//
//	so p99 — the number actually published — has a truth to hit
//
// /count also reports "duplicates": how many idempotency keys were seen more
// than once. A correct load profile must produce zero, across runs as well as
// within one.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	delayMs, _ := strconv.Atoi(os.Getenv("DELAY_MS"))
	delay := time.Duration(delayMs) * time.Millisecond
	failPct, _ := strconv.Atoi(os.Getenv("FAIL_PCT"))
	// TAIL_PCT percent of requests take TAIL_MS instead of DELAY_MS — a known
	// bimodal distribution, so the p99 the benchmark PUBLISHES can be checked
	// against a truth rather than only the p50. With TAIL_PCT=5 the true p99
	// is TAIL_MS; with TAIL_PCT=0.5 it is DELAY_MS. Chosen deterministically
	// by request counter, not random, so the proportion is exact.
	// RAMP_US adds this many microseconds per request served, so latency
	// climbs steadily through the run — a known SLOPE, to prove the drift
	// check sees one.
	rampUs, _ := strconv.Atoi(os.Getenv("RAMP_US"))
	tailPct, _ := strconv.Atoi(os.Getenv("TAIL_PCT"))
	tailMs, _ := strconv.Atoi(os.Getenv("TAIL_MS"))
	tail := time.Duration(tailMs) * time.Millisecond
	maxInflight, _ := strconv.Atoi(os.Getenv("MAX_INFLIGHT"))
	if maxInflight <= 0 {
		maxInflight = 1 << 20
	}
	sem := make(chan struct{}, maxInflight)
	var events, requests, rejected, duplicates int64

	// Idempotency-key ledger. A real ingest endpoint DEDUPLICATES a repeated
	// key, so a load profile that reuses keys across runs measures the dedupe
	// path and reports it as throughput. That is not hypothetical: ingest.js
	// keyed on `k6-<VU>-<ITER>-<seq>`, which restarts identically every run, and
	// a repeat run claimed 1,000 events ingested while writing ZERO rows.
	// Tracking duplicates here is what lets calibrate.sh catch that class.
	var keyMu sync.Mutex
	seenKeys := map[string]struct{}{}
	noteKey := func(k string) {
		if k == "" {
			return
		}
		keyMu.Lock()
		if _, dup := seenKeys[k]; dup {
			atomic.AddInt64(&duplicates, 1)
		} else {
			seenKeys[k] = struct{}{}
		}
		keyMu.Unlock()
	}

	h := func(batch bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			n := int64(1)
			type keyed struct {
				IdempotencyKey string `json:"idempotency_key"`
			}
			if batch {
				var arr []keyed
				if err := json.Unmarshal(body, &arr); err != nil {
					w.WriteHeader(400)
					return
				}
				n = int64(len(arr))
				for _, e := range arr {
					noteKey(e.IdempotencyKey)
				}
			} else {
				var one keyed
				if json.Unmarshal(body, &one) == nil {
					noteKey(one.IdempotencyKey)
				}
			}
			// Reject beyond capacity, like a real saturated server.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			default:
				atomic.AddInt64(&rejected, 1)
				w.WriteHeader(503)
				return
			}
			// the known truth: every TAIL_PCT-th percent of requests is slow
			seq := atomic.AddInt64(&requests, 0)
			if tailPct > 0 && int(seq)%100 < tailPct {
				time.Sleep(tail)
			} else {
				time.Sleep(delay + time.Duration(seq*int64(rampUs))*time.Microsecond)
			}
			if failPct > 0 && int(atomic.LoadInt64(&requests))%100 < failPct {
				atomic.AddInt64(&requests, 1)
				atomic.AddInt64(&rejected, 1)
				w.WriteHeader(500)
				return
			}
			atomic.AddInt64(&events, n)
			atomic.AddInt64(&requests, 1)
			w.WriteHeader(201)
		}
	}
	http.HandleFunc("/v1/usage-events/batch", h(true))
	http.HandleFunc("/v1/usage-events", h(false))
	// Identity check for calibrate.sh's start_stub: proves the process answering
	// on the port is the one just launched, not a stale stub from an earlier run.
	stubID := os.Getenv("STUB_ID")
	http.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, stubID) })
	http.HandleFunc("/count", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"events":%d,"requests":%d,"rejected":%d,"duplicates":%d}`,
			atomic.LoadInt64(&events), atomic.LoadInt64(&requests), atomic.LoadInt64(&rejected), atomic.LoadInt64(&duplicates))
	})
	_ = http.ListenAndServe(":8123", nil)
}
