// Ingress under load, with the acceptance criteria as thresholds.
//
// THE THRESHOLDS ARE THE GATE. k6 exits non-zero when one is breached, so a
// regression fails the build rather than appearing in a report nobody reads.
// That is the whole point of M3-07: the architecture defends itself with data
// instead of opinion.
//
// Three rungs — 10, 100, 1000 rps — run in SEQUENCE rather than together.
// Measuring 100 while 1000 is running would measure the 1000, and the whole
// value of the curve is the comparison between rungs.
//
// The curve is the point, per M3-07's own engineering note: it shows the
// synchronous ingress path degrading while the async path stays flat, which
// turns ADR-0003 from an argument into a measurement. 10 rps is the
// unsaturated baseline and carries no threshold — it is the reference the
// other two are read against.
//
// The numbers are tuned to the hardware named in docs/performance.md. An SLO
// with no stated environment is a number with no meaning.

import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';
import { body, headers, idempotencyKey } from './lib/request.js';

const BASE = __ENV.TASKING_API_URL || 'http://localhost:8080';

// Per-scenario latency, because a single trend would average the two rates
// together and report a number that describes neither.
const latencyAt10 = new Trend('ingress_latency_10rps', true);
const latencyAt100 = new Trend('ingress_latency_100rps', true);
const latencyAt1000 = new Trend('ingress_latency_1000rps', true);

export const options = {
  discardResponseBodies: false,
  scenarios: {
    at_10: {
      executor: 'constant-arrival-rate',
      rate: 10,
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 10,
      maxVUs: 50,
      exec: 'submitAt10',
      tags: { scenario: 'at_10' },
    },
    at_100: {
      executor: 'constant-arrival-rate',
      rate: 100,
      timeUnit: '1s',
      duration: '30s',
      startTime: '35s',
      // Pre-allocated generously. A constant-arrival-rate scenario that runs
      // out of VUs stops being an arrival-rate test — it silently becomes a
      // closed-loop one, and the latency it then reports is the latency of a
      // queue k6 itself created.
      preAllocatedVUs: 50,
      maxVUs: 200,
      exec: 'submitAt100',
      tags: { scenario: 'at_100' },
    },
    at_1000: {
      executor: 'constant-arrival-rate',
      // Overridable so CI can offer a rate its runner can actually generate.
      // A GitHub runner has a fraction of the cores this was measured on, and
      // a k6 that cannot produce the offered rate fails `dropped_iterations`
      // — correctly, because the thresholds would otherwise describe a load
      // that was never applied.
      //
      // Lowering it WEAKENS the claim rather than preserving it: p95 < 40ms at
      // 200 rps is a smaller statement than the same bar at 1000. The full
      // rate is what docs/performance.md reports, measured on the hardware
      // that document names.
      rate: Number(__ENV.TOP_RATE || 1000),
      timeUnit: '1s',
      duration: '30s',
      startTime: '70s',
      preAllocatedVUs: 300,
      maxVUs: 1000,
      exec: 'submitAt1000',
      tags: { scenario: 'at_1000' },
    },
  },
  thresholds: {
    // The acceptance criteria, verbatim.
    'ingress_latency_100rps': ['p(95)<15'],
    'ingress_latency_1000rps': ['p(95)<40', 'p(99)<120'],
    // Zero errors, stated as a rate rather than a count so it reads the same
    // at any duration.
    'http_req_failed': ['rate==0'],
    // A dropped iteration means k6 could not keep up with the arrival rate, so
    // the offered load was not the load the thresholds above were measured
    // against. Without this, an under-provisioned k6 makes the service look
    // fast by simply asking it for less.
    'dropped_iterations': ['count==0'],
  },
};

function submit(index, trend, prefix) {
  const key = idempotencyKey(__VU, index, prefix);
  const response = http.post(`${BASE}/v1/tasking-requests`, body(index), {
    headers: headers(key),
  });

  trend.add(response.timings.duration);

  check(response, {
    'accepted': (r) => r.status === 202,
  });
  return response;
}

export function submitAt10() {
  submit(__ITER, latencyAt10, 'k6-10');
}

export function submitAt100() {
  submit(__ITER, latencyAt100, 'k6-100');
}

export function submitAt1000() {
  submit(__ITER, latencyAt1000, 'k6-1000');
}
