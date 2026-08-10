# Load tests

k6 scenarios whose **thresholds are the gate**. Each script exits non-zero when
a threshold is breached, so a performance regression fails the build rather
than appearing in a report nobody reads.

| Script | What it measures | Thresholds |
| --- | --- | --- |
| `ingress.js` | `POST /v1/tasking-requests` at 10, 100 and 1000 rps | p95 < 15ms at 100; p95 < 40ms and p99 < 120ms at 1000; zero errors |
| `pipeline.js` | Submit to customer-visible answer, end to end | p99 < 5s; every request resolves |

```bash
make loadtest              # both, against a running stack
make loadtest-ingress      # just the ingress curve
make loadtest-pipeline     # just the end-to-end path
```

The stack must already be up and seeded: `make up-all && make seed`.

## Two things these scripts are careful about

**A unique `Idempotency-Key` per request.** Reusing one exercises the
idempotency REPLAY path — a ledger hit that returns the stored response without
touching the write path. Measuring replays and calling it ingress throughput is
the most flattering possible mistake.

**`dropped_iterations: count==0` on every scenario.** A `constant-arrival-rate`
scenario that runs out of VUs stops being an arrival-rate test and silently
becomes a closed-loop one. Without this threshold, an under-provisioned k6
makes the service look fast by asking it for less.

## The vacuous-threshold trap

`pipeline.js` asserts `pipeline_resolved_total: count>0` before it asserts any
latency. Measured: with no samples at all, k6 reports `p(99)<5000` as **passed**
and `rate==1` as **passed** at "0 out of 0". A run in which nothing whatsoever
resolved came back green.

k6 refuses `count` on a trend — *"unsupported aggregation method count on metric
of type trend"* — which is how the vacuous version came to be written in the
first place. The counter exists so the gate can assert the run produced
evidence.

## Rates are configurable because the sustainable rate is below 1 rps

`pipeline.js` takes `RATE` and `TIME_UNIT`, and `rate: 1, timeUnit: 5s` is the
only way to express 0.2 rps. That is necessary rather than fussy: at any rate
above capacity this scenario measures queue depth rather than pipeline latency.

Why 0.2 rps and not 200 — the number the issue asked for — is the subject of
`docs/performance.md`. The short version is that one feasibility worker
propagates SGP4 for nine satellites in about four seconds, so the async path
drains at roughly a quarter of a request per second while ingress accepts a
thousand.
