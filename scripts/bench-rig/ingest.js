// k6 load profile for Velox usage ingestion.
//
// This replaces the hand-rolled pacing loop in velox-bench for the PUBLISHED
// HTTP numbers. The reason is credibility rather than capability: a reader
// evaluating Velox should not have to trust our own load generator's timing
// code. They should be able to run this script and check us. Two bugs were
// already found in the hand-rolled version — latency measured from send rather
// than from when the request was due, and percentiles reported from too few
// samples — and both are classes of bug k6 does not have.
//
// velox-bench is still the right tool for the IN-PROCESS path and for
// profiling, because no external tool can call usage.Service.Ingest directly.
//
//   k6 run -e BASE=http://10.0.0.5:8080 -e KEY=vlx_secret_... ingest.js
//   k6 run -e RATE=2000 -e BATCH=50 -e DURATION=5m ... ingest.js
//
// Why constant-arrival-rate and not the default executor: it is OPEN LOOP. It
// starts iterations on a schedule regardless of whether earlier ones have
// finished, which is how real clients behave. The default (per-VU) executor
// waits for each response before sending again, so it silently slows down when
// the system does — and can therefore never discover the rate at which the
// system breaks.
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';
import { randomIntBetween } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

const BASE = __ENV.BASE;
const KEY = __ENV.KEY;
const RATE = parseInt(__ENV.RATE || '1000', 10);      // EVENTS per second
const BATCH = parseInt(__ENV.BATCH || '1', 10);       // events per request
const DURATION = __ENV.DURATION || '60s';
const CUSTOMERS = parseInt(__ENV.CUSTOMERS || '50', 10);
const METERS = parseInt(__ENV.METERS || '5', 10);
const SLO_P99 = parseInt(__ENV.SLO_P99_MS || '500', 10);

// The executor paces REQUESTS, so convert the event rate to a request rate.
const REQ_RATE = Math.max(1, Math.round(RATE / BATCH));

const eventsAccepted = new Counter('velox_events_accepted');

export const options = {
  scenarios: {
    ingest: {
      executor: 'constant-arrival-rate',
      rate: REQ_RATE,
      timeUnit: '1s',
      duration: DURATION,
      // Head-room for VUs. If k6 runs out it cannot start iterations on
      // schedule and reports dropped_iterations — which the threshold below
      // turns into a failure, because a run that quietly delivered less load
      // than requested is not a result.
      preAllocatedVUs: Math.max(20, REQ_RATE),
      maxVUs: Math.max(200, REQ_RATE * 4),
    },
  },
  thresholds: {
    // Any dropped iteration means the offered rate was not actually offered.
    // Reporting latency from such a run would describe a smaller experiment
    // than the one claimed.
    dropped_iterations: ['count==0'],
    http_req_failed: ['rate==0'],
    http_req_duration: [`p(99)<${SLO_P99}`],
  },
  // Percentiles we actually quote. k6 uses HDR histograms, so these are exact
  // to within the histogram's precision rather than an index into a sorted
  // slice.
  summaryTrendStats: ['avg', 'min', 'med', 'p(95)', 'p(99)', 'p(99.9)', 'max'],
};

const MODELS = ['gpt-4', 'claude-3-opus', 'gemini-pro', 'llama-2-70b', 'mistral-large'];
const OPS = ['input', 'output', 'embedding', 'moderation'];

function event(vu, iter, n) {
  return {
    // Spread across customers and meters. A single customer keeps every hot
    // index page and foreign-key lookup resident, which no real tenant enjoys.
    external_customer_id: `bench-customer-${String(randomIntBetween(0, CUSTOMERS - 1)).padStart(3, '0')}`,
    event_name: `bench_tokens_${String(randomIntBetween(0, METERS - 1)).padStart(3, '0')}`,
    quantity: randomIntBetween(1, 1000),
    dimensions: {
      model: MODELS[randomIntBetween(0, MODELS.length - 1)],
      operation: OPS[randomIntBetween(0, OPS.length - 1)],
      cached: randomIntBetween(0, 1) === 1,
    },
    // Unique per event. usage_events carries UNIQUE (tenant_id, livemode,
    // idempotency_key), so sending one costs an extra unique-index write —
    // a cost real clients pay and a benchmark omitting them never measures.
    // Colliding keys would exercise the REPLAY path instead, which is a
    // different measurement.
    idempotency_key: `k6-${vu}-${iter}-${n}-${randomIntBetween(0, 1e9)}`,
  };
}

export default function () {
  const params = {
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${KEY}` },
  };

  let res;
  if (BATCH === 1) {
    res = http.post(`${BASE}/v1/usage-events`, JSON.stringify(event(__VU, __ITER, 0)), params);
  } else {
    const body = [];
    for (let i = 0; i < BATCH; i++) body.push(event(__VU, __ITER, i));
    res = http.post(`${BASE}/v1/usage-events/batch`, JSON.stringify(body), params);
  }

  const ok = check(res, { 'accepted (2xx)': (r) => r.status >= 200 && r.status < 300 });
  if (ok) eventsAccepted.add(BATCH);
}

export function handleSummary(data) {
  const d = data.metrics.http_req_duration.values;
  const accepted = data.metrics.velox_events_accepted ? data.metrics.velox_events_accepted.values.count : 0;
  const dropped = data.metrics.dropped_iterations ? data.metrics.dropped_iterations.values.count : 0;
  const failed = data.metrics.http_req_failed ? data.metrics.http_req_failed.values.passes : 0;
  const secs = data.state.testRunDurationMs / 1000;

  // Achieved-vs-offered is the first thing to read. If achieved is materially
  // below target, the percentiles below describe a backlog, not an SLO.
  const lines = [
    '',
    `offered:        ${RATE} events/sec (batch ${BATCH} => ${REQ_RATE} req/sec)`,
    `accepted:       ${accepted} events in ${secs.toFixed(1)}s = ${(accepted / secs).toFixed(0)} events/sec`,
    `dropped iters:  ${dropped}${dropped > 0 ? '   <-- RATE WAS NOT DELIVERED; latency below is meaningless' : ''}`,
    `failed reqs:    ${failed}`,
    `latency/req:    med ${d.med.toFixed(1)}ms  p95 ${d['p(95)'].toFixed(1)}ms  p99 ${d['p(99)'].toFixed(1)}ms  p99.9 ${d['p(99.9)'].toFixed(1)}ms  max ${d.max.toFixed(1)}ms`,
    '',
  ];
  return { stdout: lines.join('\n') };
}
