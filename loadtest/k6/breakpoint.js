// Ramp ingress until something breaks, then stop and watch it recover.
//
// NOT A GATE. This one has no pass/fail thresholds on purpose: its output is a
// number and a failure mode, and a threshold would turn "find the limit" into
// "assert the limit has not moved". `make loadtest` does not run it.
//
// ────────────────────────────────────────────────────────────────────────────
// THE PREDICTION, WRITTEN BEFORE THE FIRST RUN
// ────────────────────────────────────────────────────────────────────────────
//
// Being wrong in public with the evidence is more credible than a retrofitted
// explanation, so this is recorded here and the result is recorded in
// docs/performance.md whichever way it goes.
//
// What is NOT predicted, because it is already measured: the pipeline's
// bottleneck. M3-07 found it — one feasibility worker at ~3.9s per request,
// 0.25 rps against ingress's 1000 — and pretending to predict a thing already
// on a graph would be theatre.
//
// The genuine unknown is INGRESS, which was flat to 1000 rps and never pushed
// to failure. The prediction:
//
//   1. The first thing to break is the tasking-api INGRESS CONNECTION POOL,
//      not the CPU and not NATS. #182 gave submit its own pool, separate from
//      background work; when offered load exceeds what that pool can turn
//      over, requests queue for a connection.
//
//   2. The failure is GRACEFUL, not a cliff, and the mechanism is
//      submitTimeout. A request that cannot get a connection within 5s is
//      refused with 503 rather than held open. So the expected signature is
//      latency climbing toward 5s and then a rising 503 rate — not a hang, not
//      a crash, and not a connection reset.
//
//   3. It happens somewhere between 3000 and 6000 rps on this hardware. Wide,
//      because it is a guess: ingress does two inserts per request and the
//      pool is small, but 1000 rps cost only 2ms median, so there is a lot of
//      headroom unaccounted for.
//
//   4. Recovery is immediate and needs no restart. Nothing in the ingress path
//      holds state across requests; the queue drains, the pool frees, and
//      latency returns to baseline within seconds. The BACKLOG of course does
//      not recover — that is the 4000:1 finding, and it is a different claim.
//
// Run: make loadtest-breakpoint

import http from 'k6/http';
import { Rate, Trend } from 'k6/metrics';
import { body, headers, idempotencyKey } from './lib/request.js';

const BASE = __ENV.TASKING_API_URL || 'http://localhost:8080';

const accepted = new Rate('breakpoint_accepted');
const refused = new Rate('breakpoint_refused_503');
const latency = new Trend('breakpoint_latency', true);

export const options = {
  discardResponseBodies: false,
  scenarios: {
    ramp: {
      executor: 'ramping-arrival-rate',
      startRate: 500,
      timeUnit: '1s',
      // Generous, because the point is to find the limit rather than to be
      // limited by k6. If dropped_iterations climbs before the service
      // degrades, the generator broke first and the run says nothing.
      preAllocatedVUs: 1000,
      maxVUs: 8000,
      stages: [
        { target: 1000, duration: '20s' },
        { target: 2000, duration: '20s' },
        { target: 4000, duration: '20s' },
        { target: 6000, duration: '20s' },
        { target: 8000, duration: '20s' },
        { target: 10000, duration: '20s' },
        // Down to nothing, then hold. THE RECOVERY HALF: a system that fails
        // and recovers cleanly is operationally very different from one that
        // needs a restart, and only this part of the test can tell you which
        // you built.
        { target: 0, duration: '10s' },
      ],
      tags: { phase: 'ramp' },
    },
    // After the ramp, a low steady rate to see whether baseline latency comes
    // back. Same request, same path, so any difference is the system's.
    recovery: {
      executor: 'constant-arrival-rate',
      rate: 50,
      timeUnit: '1s',
      duration: '30s',
      startTime: '135s',
      preAllocatedVUs: 50,
      maxVUs: 200,
      exec: 'recover',
      tags: { phase: 'recovery' },
    },
  },
};

function submit(prefix) {
  const response = http.post(`${BASE}/v1/tasking-requests`, body(__ITER), {
    headers: headers(idempotencyKey(__VU, __ITER, prefix)),
  });

  latency.add(response.timings.duration, { phase: prefix });
  accepted.add(response.status === 202);
  refused.add(response.status === 503);
  return response;
}

export default function () {
  submit('ramp');
}

export function recover() {
  submit('recovery');
}
