// k6 ingest load profile for Velox.
//
// This replaced a hand-rolled Go load generator. The reason is not taste: the
// hand-rolled pacer had a burst bug that went unnoticed for the whole first
// round of AWS measurements. Every worker computed its due times from slot 0
// of a shared schedule, so all of them fired simultaneously and then idled.
// The MEAN rate was correct, which is why nothing looked wrong, but the
// self-inflicted queueing was being counted as Velox's latency — fixing it
// moved the measured median from 24.8ms to 6.1ms. k6's constant-arrival-rate
// executor is the thing that gets this right by construction, and it is
// corrected for coordinated omission (it reports dropped iterations rather
// than quietly stretching the schedule when the system can't keep up).
//
// Two scenarios, and the difference decides whether the latency means anything:
//
//   MODE=rate   (default) — constant-arrival-rate. Requests are offered on a
//               fixed schedule regardless of how fast the system responds.
//               This is the only mode whose percentiles are an SLO. Watch
//               `dropped_iterations`: any non-zero value means the offered
//               rate exceeded what the rig could sustain, and the run is a
//               failed attempt at that rate, not a measurement of it.
//   MODE=max    — closed-loop, fixed VUs sending as fast as responses return.
//               Finds a ceiling. Its p99 describes how deep the queue got, so
//               it is a maximum, never a service-level number.
//
// Environment:
//   BASE        base URL of the running velox server (required)
//   API_KEY     Bearer key from `velox-bench-seed` (required)
//   CUSTOMER    external_customer_id from velox-bench-seed (default bench-customer)
//   EVENT       event_name from velox-bench-seed (default bench_tokens)
//   BATCH       events per request; 1 uses the single-event endpoint (default 1)
//   RATE        offered events/sec in rate mode (default 200)
//   VUS         concurrency. In rate mode this is a pool, not the load itself.
//   DURATION    e.g. 90s, 10m (default 60s)
//   P99_MS      pass/fail p99 budget in ms; the run fails the threshold if missed
//
// Usage:
//   k6 run -e BASE=http://localhost:8080 -e API_KEY=vlx_test_… \
//          -e RATE=1000 -e BATCH=10 -e DURATION=10m ingest.js

import http from 'k6/http'
import { check } from 'k6'
import { Counter } from 'k6/metrics'

const BASE = __ENV.BASE
const API_KEY = __ENV.API_KEY
const CUSTOMER = __ENV.CUSTOMER || 'bench-customer'
const EVENT = __ENV.EVENT || 'bench_tokens'
const BATCH = parseInt(__ENV.BATCH || '1', 10)
const RATE = parseInt(__ENV.RATE || '200', 10)
const DURATION = __ENV.DURATION || '60s'
const P99_MS = parseInt(__ENV.P99_MS || '0', 10)
const MODE = __ENV.MODE || 'rate'

if (!BASE || !API_KEY) {
  throw new Error('BASE and API_KEY are required — run cmd/velox-bench-seed to obtain API_KEY')
}

// Events per second, not requests per second. A batch of 10 at 100 requests/s
// is 1,000 events/s, and the events/s figure is the one that gets published,
// so the arrival rate is derived rather than left for the reader to multiply.
const REQ_RATE = Math.max(1, Math.ceil(RATE / BATCH))

// Sized by Little's Law against the TAIL, not the median, and deliberately
// generous. Under constant-arrival-rate a VU is a capacity pool, not the load:
// the offered rate is fixed, so a spare VU costs memory and nothing else. Size
// it too tightly and every request landing in the p99 tail holds a VU long
// enough to starve the schedule, and k6 reports dropped iterations — which
// reads as "Velox could not sustain this rate" when the truth is that the
// generator could not offer it.
//
// That is not hypothetical. This script's first draft used REQ_RATE/10 (a
// 100ms tail budget) and reported 7 dropped iterations at 100 ev/s against a
// server whose p50 was 7.4ms; raising VUs to 50 with nothing else changed
// dropped 0. Budget 500ms per request instead.
//
// The opposite lesson from the AWS run — that raising the generator from 6 to
// 24 workers made p50 worse — came from a CLOSED-LOOP generator, where each
// worker adds offered load. It does not transfer to this executor. In `max`
// mode below it does, which is why VUS is the load there and must be chosen.
const VUS = parseInt(__ENV.VUS || String(Math.min(512, Math.max(16, Math.ceil(REQ_RATE * 0.5)))), 10)

export const options = {
  discardResponseBodies: true,
  // k6 computes p(90) and p(95) by default and NOTHING else — asking for
  // values.['p(99)'] without this line silently yields undefined, which prints
  // as a confident 0.0ms. The first run of this script did exactly that.
  summaryTrendStats: ['min', 'med', 'avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
  scenarios: MODE === 'max'
    ? {
        ceiling: {
          executor: 'constant-vus',
          vus: VUS,
          duration: DURATION,
        },
      }
    : {
        offered: {
          executor: 'constant-arrival-rate',
          rate: REQ_RATE,
          timeUnit: '1s',
          duration: DURATION,
          preAllocatedVUs: VUS,
          // A hard cap, deliberately. Letting k6 grow VUs without bound turns
          // an overload into a slow-motion closed-loop run instead of the
          // dropped_iterations signal that says "this rate was not sustained".
          maxVUs: VUS * 4,
        },
      },
  thresholds: {
    // A run with errors is not a slower run, it is a different run. Fail it.
    http_req_failed: ['rate==0'],
    ...(P99_MS > 0 ? { http_req_duration: [`p(99)<${P99_MS}`] } : {}),
  },
}

const eventsIngested = new Counter('events_ingested')

// Distinguishes this run's idempotency keys from every previous run's. Set
// RUN_ID explicitly to make a run reproducible; otherwise it is derived per VU,
// which is enough because __VU is part of the key too.
const RUN_ID = __ENV.RUN_ID || `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}`

const params = {
  headers: {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${API_KEY}`,
  },
}

// ~80 distinct combinations, matching the cardinality the multi-dimensional
// meter design doc assumes. A single-value hammer would let Postgres cache
// one JSONB shape and report a throughput no real tenant would see.
const MODELS = ['gpt-4', 'gpt-4-turbo', 'gpt-3.5-turbo', 'claude-3-opus', 'claude-3-sonnet',
  'claude-3-haiku', 'gemini-pro', 'llama-2-70b', 'mistral-large', 'command-r']
const OPERATIONS = ['input', 'output', 'embedding', 'moderation']

function pick(a) { return a[Math.floor(Math.random() * a.length)] }

function event(seq) {
  return {
    external_customer_id: CUSTOMER,
    event_name: EVENT,
    quantity: 1 + Math.floor(Math.random() * 1000),
    // Unique per event across VUs, iterations AND RUNS. RUN_ID is the part
    // that was missing: `k6-<VU>-<ITER>-<seq>` is fully deterministic, so every
    // run after the first replays the same keys, the server correctly
    // deduplicates them, and the run measures the DEDUPE path while reporting
    // it as ingest throughput. Measured before the fix: a repeat run claimed
    // 1,000 events ingested and wrote ZERO rows.
    idempotency_key: `k6-${RUN_ID}-${__VU}-${__ITER}-${seq}`,
    dimensions: {
      model: pick(MODELS),
      operation: pick(OPERATIONS),
      cached: Math.random() < 0.5,
    },
  }
}

export default function () {
  let res
  if (BATCH === 1) {
    res = http.post(`${BASE}/v1/usage-events`, JSON.stringify(event(0)), params)
  } else {
    const events = new Array(BATCH)
    for (let i = 0; i < BATCH; i++) events[i] = event(i)
    res = http.post(`${BASE}/v1/usage-events/batch`, JSON.stringify(events), params)
  }

  const ok = check(res, { 'ingest accepted': (r) => r.status === 201 || r.status === 200 })
  if (ok) eventsIngested.add(BATCH)
}

export function handleSummary(data) {
  const m = data.metrics
  const dropped = m.dropped_iterations ? m.dropped_iterations.values.count : 0
  const ingested = m.events_ingested ? m.events_ingested.values.count : 0
  const secs = m.http_req_duration ? data.state.testRunDurationMs / 1000 : 0
  const d = m.http_req_duration ? m.http_req_duration.values : {}

  const lines = [
    '',
    `mode              ${MODE}${MODE === 'rate' ? ` (offered ${RATE} ev/s = ${REQ_RATE} req/s)` : ''}`,
    `batch             ${BATCH}`,
    `events ingested   ${ingested}`,
    `events/sec        ${secs > 0 ? (ingested / secs).toFixed(0) : 'n/a'}`,
    `requests failed   ${m.http_req_failed ? (m.http_req_failed.values.passes || 0) : 0}`,
    `latency p50/p99   ${(d['p(50)'] || 0).toFixed(1)}ms / ${(d['p(99)'] || 0).toFixed(1)}ms`,
  ]
  // Only meaningful in rate mode, and the single most important line there:
  // a rate with dropped iterations was NOT sustained, whatever its p99 says.
  if (MODE === 'rate') {
    lines.push(`dropped           ${dropped}${dropped > 0 ? '  <-- RATE NOT SUSTAINED' : ''}`)
  }
  lines.push('')

  return {
    stdout: lines.join('\n'),
    'summary.json': JSON.stringify(data, null, 2),
  }
}
