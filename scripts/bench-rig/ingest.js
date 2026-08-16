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
//   CUSTOMERS   number of bench customers velox-bench-seed created (default 200).
//               Each REQUEST picks one at random; a batch stays single-customer,
//               matching the batch handler's own design assumption. The probe
//               reads a random customer too.
//   CUSTOMER_PREFIX / CUSTOMER_ID_PREFIX  id prefixes from velox-bench-seed
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
const CUSTOMERS = parseInt(__ENV.CUSTOMERS || '200', 10)
const CUSTOMER_PREFIX = __ENV.CUSTOMER_PREFIX || 'bench-customer-'
const CUSTOMER_ID_PREFIX = __ENV.CUSTOMER_ID_PREFIX || 'vlx_cus_bench_'
const EVENT = __ENV.EVENT || 'bench_tokens'

// Numbered ids, zero-padded to 3, matching velox-bench-seed. Picking a customer
// per request rather than using one for the whole run is what makes this a
// benchmark and not a pathological case: one customer means the resolve cache
// never misses, the btrees append to a single hot edge, and the probe's
// usage-summary aggregates the entire table for that one customer.
function pad3(n) { return String(n).padStart(3, '0') }
function pickCustomer() { return Math.floor(Math.random() * CUSTOMERS) }
const BATCH = parseInt(__ENV.BATCH || '1', 10)
const RATE = parseInt(__ENV.RATE || '200', 10)
const DURATION = __ENV.DURATION || '60s'
const P99_MS = parseInt(__ENV.P99_MS || '0', 10)
const MODE = __ENV.MODE || 'rate'
// Responsiveness probe. Set PROBE_RATE>0 to run a small INTERACTIVE workload
// concurrently with the ingest load, with its own latency threshold.
//
// This is the half of the benchmark that a buyer actually experiences. Nobody
// experiences "10,203 events/sec"; a finance team experiences whether the
// invoice page loads on the 1st of the month while ingest is at peak. A
// throughput number with no concurrent SLO on the read path is the number that
// flatters the vendor, which is why it is the one vendors publish.
const PROBE_RATE = parseInt(__ENV.PROBE_RATE || '0', 10)
const PROBE_P99_MS = parseInt(__ENV.PROBE_P99_MS || '500', 10)
// CUSTOMER_ID pins the probe to one customer; unset, the probe picks at random
// from the same pool ingest writes to, which is what a dashboard user does.
const CUSTOMER_ID = __ENV.CUSTOMER_ID || ''

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

const INGEST_SCENARIO = MODE === 'max' ? 'ceiling' : 'offered'

// Built as a plain object rather than inline in `options`, because a spread
// placed after the scenarios ternary lands at the TOP level of options — k6
// rejects it with "unknown field: probe" rather than running one scenario.
const scenarios = MODE === 'max'
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
    }

if (PROBE_RATE > 0) {
  scenarios.probe = {
    executor: 'constant-arrival-rate',
    exec: 'probe',
    rate: PROBE_RATE,
    timeUnit: '1s',
    duration: DURATION,
    preAllocatedVUs: Math.max(4, PROBE_RATE * 2),
    maxVUs: Math.max(16, PROBE_RATE * 8),
  }
}

export const options = {
  discardResponseBodies: true,
  // k6 computes p(90) and p(95) by default and NOTHING else — asking for
  // values.['p(99)'] without this line silently yields undefined, which prints
  // as a confident 0.0ms. The first run of this script did exactly that.
  summaryTrendStats: ['count', 'min', 'med', 'avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
  scenarios,
  thresholds: {
    // A run with errors is not a slower run, it is a different run. Fail it.
    http_req_failed: ['rate==0'],
    // Declaring a threshold on a tagged metric is what makes k6 CREATE that
    // sub-metric, so these two exist even with an always-true expression. The
    // ingest scenario's latency and drops are then readable in isolation —
    // without this, `latency p50/p99` was the GLOBAL http_req_duration, and
    // with PROBE_RATE>0 the probe's read latency was pooled into the published
    // ingest tail (13% of samples at batch=10, ~43% at batch=500).
    [`http_req_duration{scenario:${INGEST_SCENARIO}}`]: P99_MS > 0 ? [`p(99)<${P99_MS}`] : ['p(99)>=0'],
    [`dropped_iterations{scenario:${INGEST_SCENARIO}}`]: ['count>=0'],
    // Scoped to the probe scenario by k6's built-in `scenario` tag. Declaring
    // the threshold is also what CREATES the sub-metric the summary reads.
    // This is what makes the responsiveness claim falsifiable: if reads degrade
    // past the budget under write load, the run FAILS rather than publishing a
    // throughput number alongside an unusable product.
    ...(PROBE_RATE > 0
      ? { 'http_req_duration{scenario:probe}': [`p(99)<${PROBE_P99_MS}`] }
      : {}),
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

function event(seq, customer) {
  return {
    external_customer_id: CUSTOMER_PREFIX + pad3(customer),
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
  const customer = pickCustomer()
  if (BATCH === 1) {
    res = http.post(`${BASE}/v1/usage-events`, JSON.stringify(event(0, customer)), params)
  } else {
    const events = new Array(BATCH)
    for (let i = 0; i < BATCH; i++) events[i] = event(i, customer)
    res = http.post(`${BASE}/v1/usage-events/batch`, JSON.stringify(events), params)
  }

  const ok = check(res, { 'ingest accepted': (r) => r.status === 201 || r.status === 200 })
  if (ok) eventsIngested.add(BATCH)
}

// The reads a human actually waits on. usage-summary is deliberately first and
// weighted heaviest: it AGGREGATES the very table ingest is writing to, so it
// is where write load shows up first.
export function probe() {
  const now = new Date()
  const from = new Date(now.getTime() - 30 * 24 * 3600 * 1000).toISOString()
  const to = now.toISOString()

  const cid = CUSTOMER_ID || CUSTOMER_ID_PREFIX + pad3(pickCustomer())
  const r1 = http.get(`${BASE}/v1/usage-summary/${cid}?from=${from}&to=${to}`, params)
  const r2 = http.get(`${BASE}/v1/invoices?limit=20`, params)
  const r3 = http.get(`${BASE}/v1/customers?limit=20`, params)
  // A read that errors is a failed probe, not a fast one. http_req_failed
  // already covers 4xx/5xx globally; the check makes it visible per endpoint.
  check(r1, { 'usage-summary 200': (r) => r.status === 200 })
  check(r2, { 'invoices 200': (r) => r.status === 200 })
  check(r3, { 'customers 200': (r) => r.status === 200 })
}

// '90s' | '10m' | '1h' | '1h30m' -> seconds
function parseDuration(d) {
  let total = 0
  const re = /(\d+)([hms])/g
  let mm
  while ((mm = re.exec(d)) !== null) {
    const n = parseInt(mm[1], 10)
    total += mm[2] === 'h' ? n * 3600 : mm[2] === 'm' ? n * 60 : n
  }
  return total
}

export function handleSummary(data) {
  const m = data.metrics
  const scoped = (name) => (m[`${name}{scenario:${INGEST_SCENARIO}}`] || m[name] || {}).values || {}
  const dropped = scoped('dropped_iterations').count || 0
  const ingested = m.events_ingested ? m.events_ingested.values.count : 0
  // Denominator is the OFFERED window in rate mode, not testRunDurationMs:
  // that figure includes k6's gracefulStop tail (up to 30s waiting for
  // in-flight iterations), so under a slow server 200 events over a 10s window
  // printed as 17 ev/s instead of 20. Wrong in the safe direction, but wrong.
  // In max mode there is no schedule, so wall-clock is the honest denominator.
  const durSecs = parseDuration(DURATION)
  const secs = MODE === 'rate' && durSecs > 0 ? durSecs : (m.http_req_duration ? data.state.testRunDurationMs / 1000 : 0)
  // INGEST-scenario latency only — never pooled with the probe.
  const d = scoped('http_req_duration')

  const lines = [
    '',
    `mode              ${MODE}${MODE === 'rate' ? ` (offered ${RATE} ev/s = ${REQ_RATE} req/s)` : ''}`,
    `batch             ${BATCH}`,
    `events ingested   ${ingested}`,
    `events/sec        ${secs > 0 ? (ingested / secs).toFixed(0) : 'n/a'}`,
    `requests failed   ${m.http_req_failed ? (m.http_req_failed.values.passes || 0) : 0}`,
    `latency samples   ${d.count || 0}${(d.count || 0) < 1000 ? '  <-- FEWER THAN 1000: p99 is not trustworthy' : ''}`,
    `latency p50/p99   ${(d['p(50)'] || 0).toFixed(1)}ms / ${(d['p(99)'] || 0).toFixed(1)}ms`,
  ]
  // Only meaningful in rate mode, and the single most important line there:
  // a rate with dropped iterations was NOT sustained, whatever its p99 says.
  if (MODE === 'rate') {
    lines.push(`dropped           ${dropped}${dropped > 0 ? '  <-- RATE NOT SUSTAINED' : ''}`)
  }
  if (PROBE_RATE > 0) {
    const pd = (m['http_req_duration{scenario:probe}'] || {}).values || {}
    lines.push('')
    lines.push(`READ PROBE (concurrent, ${PROBE_RATE}/s)`)
    lines.push(`  p50/p99        ${(pd['p(50)'] || 0).toFixed(1)}ms / ${(pd['p(99)'] || 0).toFixed(1)}ms  (budget p99 < ${PROBE_P99_MS}ms)`)
    lines.push(`  verdict        ${(pd['p(99)'] || 0) < PROBE_P99_MS ? 'RESPONSIVE under load' : 'DEGRADED — read path missed its budget'}`)
  }
  lines.push('')

  return {
    stdout: lines.join('\n'),
    'summary.json': JSON.stringify(data, null, 2),
  }
}
