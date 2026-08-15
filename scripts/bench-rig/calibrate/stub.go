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
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

func main() {
	delayMs, _ := strconv.Atoi(os.Getenv("DELAY_MS"))
	delay := time.Duration(delayMs) * time.Millisecond
	failPct, _ := strconv.Atoi(os.Getenv("FAIL_PCT"))
	maxInflight, _ := strconv.Atoi(os.Getenv("MAX_INFLIGHT"))
	if maxInflight <= 0 {
		maxInflight = 1 << 20
	}
	sem := make(chan struct{}, maxInflight)
	var events, requests, rejected int64

	h := func(batch bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			n := int64(1)
			if batch {
				var arr []json.RawMessage
				if err := json.Unmarshal(body, &arr); err != nil {
					w.WriteHeader(400)
					return
				}
				n = int64(len(arr))
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
			time.Sleep(delay) // the known truth
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
	http.HandleFunc("/count", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"events":%d,"requests":%d,"rejected":%d}`, atomic.LoadInt64(&events), atomic.LoadInt64(&requests), atomic.LoadInt64(&rejected))
	})
	_ = http.ListenAndServe(":8123", nil)
}
