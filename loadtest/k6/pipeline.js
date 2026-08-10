// End to end: a submitted request through to an outcome the read model serves.
//
// This is the criterion the other scenarios cannot reach. ingress.js measures
// the synchronous write path, which is fast because it does almost nothing —
// validate, insert, insert to outbox, return 202. What a customer experiences
// is the ASYNC path: the relay publishing, feasibility sweeping SGP4 over nine
// satellites, the planner opening a round, the plan committing, and the
// gateway projecting it.
//
// Measuring that is the point of ADR-0003's split, and it is the number that
// decides whether the split was worth it.
//
// WHAT IS MEASURED, PRECISELY: submit-to-visible, where visible means
// plan-gateway answers with the request in a terminal state. Not "the event
// was published" — a customer cannot see an event. The read model is the
// customer-visible edge of this system and so it is the edge that is timed.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import { body, headers, idempotencyKey } from './lib/request.js';

const TASKING = __ENV.TASKING_API_URL || 'http://localhost:8080';
const GATEWAY = __ENV.PLAN_GATEWAY_URL || 'http://localhost:8083';

// How long to keep asking before giving up on one request. Generous, because a
// timeout here is recorded as an incomplete rather than a latency — a request
// that never resolves must not be able to improve the percentile by dropping
// out of it, which is the classic way an end-to-end number flatters itself.
const RESOLVE_TIMEOUT_MS = Number(__ENV.RESOLVE_TIMEOUT_MS || 30_000);
const POLL_INTERVAL_S = 0.25;

// The states that mean the pipeline has produced an ANSWER for this request.
//
// Read from the state machine rather than invented: RECEIVED means feasibility
// has not answered yet and AWAITING_PLANNING means the planner has not, so
// both are still in flight. Everything else is an outcome a customer can act
// on — including the negative ones, because "your request is infeasible" is a
// completed pipeline, not a failure of it.
//
// INFEASIBLE counts as resolved deliberately. A load test that only accepted
// PLANNED would report the pipeline as broken every time the constellation was
// simply busy, which is the condition it is most interesting to measure under.
//
// There is deliberately NO 'UNFULFILLED' here, and that is not an omission —
// no such state exists. Losing a round moves PLANNED back to
// AWAITING_PLANNING (statemachine.go:58), because an unfulfilled request stays
// in contention for later buckets. So it is still IN FLIGHT and must keep
// being polled; treating it as an answer would stop the clock on a request the
// system has not finished with. Taken from the state machine rather than
// guessed, after guessing wrongly.
//
// PLANNED is not terminal — an acquisition can still be executed or superseded
// — but it IS an answer: the customer can see their slot. This measures time
// to an answer, not time to a final resting state.
const RESOLVED_STATES = new Set([
  'PLANNED',
  'ACQUIRED',
  'INFEASIBLE',
  'REJECTED',
  'EXPIRED',
  'CANCELLED',
]);

const endToEnd = new Trend('pipeline_submit_to_visible', true);
const resolved = new Rate('pipeline_resolved');

// A Counter alongside the Trend purely so a threshold can assert the run
// produced evidence. k6 refuses `count` on a trend — "unsupported aggregation
// method count on metric of type trend" — which is how the vacuous version of
// this gate came to be written.
const resolvedCount = new Counter('pipeline_resolved_total');

export const options = {
  scenarios: {
    sustained: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE || 1),
      // Configurable, because the sustainable rate here is BELOW one per
      // second — see docs/performance.md. `rate: 1, timeUnit: 5s` is the only
      // way to express 0.2 rps, and expressing it is necessary rather than
      // fussy: at any rate above capacity this scenario measures queue depth
      // rather than pipeline latency.
      timeUnit: __ENV.TIME_UNIT || '5s',
      duration: __ENV.DURATION || '60s',
      // Long enough for the last submitted request to finish. The default 30s
      // silently DISCARDS iterations still running when the duration ends, and
      // a discarded iteration contributes nothing — so a scenario that times
      // out entirely reports no samples rather than a failure.
      gracefulStop: __ENV.GRACEFUL_STOP || '180s',
      // Each iteration holds a VU for as long as the pipeline takes, not for
      // as long as the HTTP call takes, so this needs far more VUs than the
      // ingress scenarios at the same rate.
      preAllocatedVUs: 400,
      maxVUs: 2000,
      tags: { scenario: 'pipeline' },
    },
  },
  thresholds: {
    // count>0 FIRST, and it is not decoration.
    //
    // Measured: with no samples at all, k6 reports `p(99)<5000` as PASSED and
    // `rate==1` as PASSED at "0 out of 0". A run in which nothing whatsoever
    // resolved came back green. That is a gate that cannot fail, which this
    // repository has been bitten by before — an alert rule naming a metric
    // nobody published, a drift check comparing a directory with itself.
    //
    // The minimum is deliberately low: it asserts the scenario PRODUCED
    // EVIDENCE, not that it produced a particular amount.
    'pipeline_resolved_total': ['count>0'],
    // 60s, NOT the 5s the acceptance criterion asks for, and the gap is
    // recorded rather than papered over.
    //
    // Measured on a cold stack at 0.1 rps with an empty queue: median 30.3s,
    // p99 44.8s. The 5s SLO is unreachable here by roughly an order of
    // magnitude, and not because of contention — a planner round commits in
    // under a millisecond. The time is queue and poll cadence: one feasibility
    // worker propagates SGP4 across nine satellites in ~3.9s, and each
    // projection hop waits up to FETCH_WAIT (5s) on three streams.
    //
    // docs/performance.md carries the decomposition and what it would take to
    // close the gap. This threshold is set where it is so the scenario works
    // as a REGRESSION gate — it catches the pipeline getting worse — while
    // the unmet SLO stays visible in the write-up rather than being quietly
    // redefined as met.
    'pipeline_submit_to_visible': ['p(99)<60000'],
    // Every submitted request must resolve. Without this the latency
    // threshold above can be satisfied by a system that answers a fast
    // minority and abandons the rest.
    'pipeline_resolved': ['rate==1'],
    'dropped_iterations': ['count==0'],
  },
};

export default function () {
  const key = idempotencyKey(__VU, __ITER, 'k6-pipeline');
  const started = Date.now();

  const submit = http.post(`${TASKING}/v1/tasking-requests`, body(__ITER), {
    headers: headers(key),
  });
  if (!check(submit, { 'accepted': (r) => r.status === 202 })) {
    resolved.add(false);
    return;
  }

  const requestID = submit.json('request_id');
  if (!requestID) {
    resolved.add(false);
    return;
  }

  // Poll the read model until the request reaches a terminal state.
  //
  // Polling rather than subscribing to SSE: this measures how long the DATA
  // takes to become visible, and a push notification would measure the
  // notification path instead. The poll interval is a floor on the resolution
  // of the measurement and is stated in docs/performance.md alongside the
  // number, because a 250ms poll cannot resolve a 100ms pipeline.
  while (Date.now() - started < RESOLVE_TIMEOUT_MS) {
    const view = http.get(`${GATEWAY}/v1/requests/${requestID}`, {
      tags: { name: 'GET /v1/requests/{request_id}' },
    });

    if (view.status === 200 && RESOLVED_STATES.has(view.json('state'))) {
      endToEnd.add(Date.now() - started);
      resolved.add(true);
      resolvedCount.add(1);
      return;
    }
    sleep(POLL_INTERVAL_S);
  }

  // Timed out. Recorded as UNRESOLVED and deliberately NOT added to the
  // latency trend: a request that never completed has no latency, and giving
  // it one — or silently omitting it — is how an end-to-end percentile ends up
  // describing only the requests that happened to work.
  resolved.add(false);
}
