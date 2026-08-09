# RED and domain metrics with Grafana dashboards — design (#53, M3-06)

Approved 2026-08-10.

## What is already here, and what is a lie

Half of this issue is wiring that already exists and has never carried a single
sample. Reading it first changes what the work is.

**The collector already has a metrics pipeline.** `deploy/otel/collector.yaml`
receives OTLP, batches, and re-exposes on `:8889` with
`resource_to_telemetry_conversion` enabled, so OTel resource attributes arrive
as Prometheus labels without a join. Prometheus already scrapes it. Nothing
about the transport needs building; something needs to *send*.

**The alert rules already name metrics nothing publishes.** Three of the four
rules in `deploy/prometheus/rules/alerts.yml` reference
`overpass_outbox_pending_seconds`, `overpass_tle_age_hours`, and
`overpass_allocation_duration_ms_bucket`. None of those series exist. Those
rules cannot fire, which is the exact failure the `overpass_dlq_depth` comment
in that same file already describes and thought it had finished fixing:

> an alert that could not fire, which is worse than no alert because it looks
> like coverage.

It looked like coverage because the DLQ rule was fixed and its neighbours were
not. **These three names are therefore fixed points, not proposals** — the
instruments are named to produce them, and if the exporter refuses, the rule
changes and this document says why.

**The domain numbers are already computed.** `planMetrics` in
`services/planner/internal/app/plan.go` builds total plan value, requests
fulfilled, requests unfulfilled, satellite utilisation ratio, and allocation
duration for every committed plan, because the `planning.plan.committed.v1`
contract requires them. They are serialised onto an event and then dropped on
the floor as far as an operator is concerned.

**Two services have a metrics struct wired to nothing.** `lib/go/consume`'s
`Metrics` is documented as being "in the shape M3-06 will scrape".
`services/planner/cmd/planner/main.go:208` reads:

```go
// projector.Metrics is served to M3-06 when the metrics endpoint lands
// (#53); until then the snapshot keeps the field exercised without copying
// the mutex it guards.
_ = projector.Metrics.Snapshot()
```

That line is the issue, in one statement.

**And one job in `prometheus.yml` is a fiction.** The `overpass-services` job
scrapes `/metrics` on `tasking-api:8080`, `plan-gateway:8081`, `planner:8082`
and `feasibility:8000`. No service serves that path. `feasibility` has no HTTP
listener at all, deliberately — `docker-compose.yml` says so:

> No ports and no healthcheck: the worker is a consumer with no listener, so
> there is nothing to probe short of inventing an HTTP server for the probe's
> benefit.

## Decision 1 — OTLP push, and the scrape job is deleted

Application metrics reach Prometheus by one path: the OTel SDK pushes OTLP to
the collector, Prometheus scrapes the collector. The `overpass-services` job is
removed.

*Rejected:* per-service `/metrics` with the Prometheus client library. It means
inventing an HTTP server inside the Python worker purely to be scraped — the
thing the compose file already refused to do for a readiness probe, and the
reason is not weaker for metrics than it was for health. It also means a second
metrics stack living next to the OTel SDK, with two naming conventions and two
places to configure a label.

*Rejected:* both, with a thin `/metrics` on the three Go services to preserve
`up{}`. Two export paths for the same numbers is how a metric ends up
double-counted and how a config stops explaining itself.

**What this costs, stated rather than buried:** there is no per-service `up{}`
series. A dead service stops sending, its gauges go stale after five minutes,
and its counters simply stop advancing. Liveness detection stays where it
already is — compose healthchecks on the three HTTP services, restart counts
and logs on the worker. This is a real loss and it is the price of one uniform
instrumentation path across two languages and a process with no listener.

Leaving the job in place was never an option. Four targets permanently `down`
is not a partial signal, it is a false one, and the first person to open
Prometheus reads it as an outage.

This supersedes the note in `2026-08-09-breaker-and-bulkhead-design.md` that
#51's breaker gauge "needs the `/metrics` surface #53 brings". There is no
`/metrics` surface. The breaker gauge lands as an OTLP-exported observable
gauge on the same path as everything else.

## Decision 2 — `lib/go/telemetry`, a new module

The three Go services need a MeterProvider. `tasking-api` has
`internal/telemetry`; `planner` and `plan-gateway` have nothing at all.

A new module, `lib/go/telemetry`, in by `replace` directive. The reasoning is
`lib/go/consume`'s, verbatim: it is shared by services that deploy separately,
so a shared module means no publishing step and no version skew. It installs
**both** the tracer and the meter provider, and `tasking-api`'s
`internal/telemetry` migrates onto it, keeping only its own `ScopeName`.

*Rejected:* a metrics-only shared module with tracing left alone. Smaller diff,
but `tasking-api` then carries two telemetry packages that both configure a
resource and both must agree about `service.name`, until #52 merges them. Two
things that must agree and are not the same code is a defect waiting for a
version bump.

*Rejected:* per-service `internal/telemetry`, duplicated three times. About 150
lines of provider setup, copied — including the `resource.Merge` schema-URL
comment that exists because getting it wrong silently disabled tracing once
already. Three copies means three chances to fix that in only two of them.

**Scope honesty:** giving `plan-gateway` a MeterProvider and `otelhttp` also
gives it server spans, which is #52's acceptance criterion, not this one. The
alternative is hand-rolling a metrics-only HTTP middleware so as not to acquire
a span, which is worse code written to keep an issue boundary tidy. It lands
here and the PR body says it lands here.

The meter uses a `PeriodicReader` at 10s, matching `scrape_interval`. Faster
would export samples Prometheus never reads; slower would make a 10s scrape
report the same value twice and flatten a rate.

## Decision 3 — RED is two shapes, because there are two kinds of work

There is no single RED shape here. Two of the four services serve no HTTP at
all, and their unit of work is a message. Forcing one abstraction over a request
and a message would produce a metric that describes neither.

**HTTP — `tasking-api`, `plan-gateway`.** The semconv instrument
`http.server.request.duration`, emitted by `otelhttp`. `tasking-api` already
wraps its router, so its RED appears the moment a MeterProvider exists.
`plan-gateway` gets the wrapper added. Rate, errors and duration all come off
one histogram sliced by `http.route` and `http.response.status_code`.

Health probes stay excluded, by the filter already in place. A liveness check
every five seconds is the highest-volume and least interesting operation the
service performs, and including it makes the error ratio meaningless — a
denominator dominated by probes hides a 100% failure rate on the one route that
matters.

**Consumers — the planner projector, the plan-gateway projector, the
feasibility worker.** One histogram:

```
overpass.consume.duration_ms{service, subject, outcome}
  outcome ∈ processed | duplicate | terminated | deadlettered | failed
```

Its `_count` is the rate, the `outcome` label is the errors, the buckets are
the duration. This is receive-to-ack, which is what `consume.Metrics.AckAfter`
already measures and what bounds how long a crash window can silently
redeliver.

Plus one counter that does not fit that shape:

```
overpass.consume.redeliveries{service, subject}
```

Redelivery is orthogonal to outcome — a redelivered message can still be
processed — and `lib/go/consume/metrics.go` already argues why it is the
load-bearing early-warning line: climbing redeliveries against flat throughput
is a poison message or a dying dependency, visible before `max_deliver` turns it
into a loss.

`consume.Metrics` keeps its current API and its `Snapshot`. The instruments go
behind the existing `Processed`/`Duplicate`/`Redelivered`/`Terminated`/
`Deadlettered`/`AckAfter` calls, so every call site is already correct and the
diff at the call sites is empty. `main.go:208`'s discarded snapshot becomes a
real export.

## Decision 4 — the domain metrics

| Metric | Type | Labels | Source |
| --- | --- | --- | --- |
| `overpass_requests_unfulfilled_total` | counter | `reason_code` | planner, one per unfulfilment |
| `overpass_requests_fulfilled_total` | counter | — | planner, per acquisition |
| `overpass_allocation_duration_ms` | histogram | `policy` | `planMetrics.AllocationDurationMs` |
| `overpass_plan_value_credits` | histogram | `policy` | `planMetrics.TotalPlanValueCredits` |
| `overpass_satellite_utilisation_ratio` | gauge | `satellite_id` | `planMetrics.SatelliteUtilisationRatio` |
| `overpass_round_candidate_opportunities` | histogram | — | `planMetrics.CandidateOpportunityCount` |
| `overpass_opportunities_per_request` | histogram | — | feasibility pipeline, per computed request |
| `overpass_feasibility_refusals_total` | counter | `reason` | feasibility refusal path |
| `overpass_tle_age_hours` | gauge | `satellite_id` | feasibility sweep |
| `overpass_outbox_pending_seconds` | gauge | `service` | relay, all three producers |
| `overpass_outbox_published_total` | counter | `service`, `outcome ∈ published \| failed` | relay |

Names are given in Prometheus form. The instruments are declared in OTel form —
`overpass.requests.unfulfilled`, `overpass.tle.age_hours` — and the exporter
performs the translation, which is the subject of the naming risk below.

**`requests_unfulfilled_total` is the point of the issue** and it is the reason
`requests_fulfilled_total` is next to it. A count of losses with no denominator
cannot distinguish a contended constellation from a busy one; the ratio is the
metric, the counter is half of it.

The seven `reason_code` values come from the contract enum, unchanged:
`LOST_TO_HIGHER_VALUE`, `BLOCKED_BY_SLEW_CONSTRAINT`, `DUTY_CYCLE_EXHAUSTED`,
`DEADLINE_PASSED`, `NO_OPPORTUNITY_IN_BUCKET`, `SUPERSEDED`,
`CANCELLED_BY_CUSTOMER`. The schema's own description already states why this
is worth charting: the first three have three completely different remedies —
bid more, widen the window, or wait for duty cycle — and a system that reports
"unfulfilled: 42" tells a customer nothing they can act on.

`overpass_feasibility_refusals_total` is the ingress-side sibling. A request
that never produced an opportunity never reaches a round and never becomes an
unfulfilment, so without it the two counts do not reconcile and requests appear
to vanish.

**TLE staleness as a per-satellite gauge, not a histogram.** The issue says
"distribution". With nine satellites in `testdata/tle`, nine labelled gauges
*are* the distribution, and they answer the question a histogram cannot: *which*
element set is old. `max by (satellite_id)` still gives the aggregate.

**Cardinality is bounded on purpose.** `reason_code` 7, `satellite_id` 9,
`policy` 4, `subject` ~6, `service` 4. No `customer_id`, no `request_id`, no
`round_id` — those are trace attributes, and putting an unbounded identifier in
a label is how a Prometheus instance is killed by its own instrumentation.

### The naming risk

The OTel-to-Prometheus exporter appends unit suffixes and `_total`, by rules
this document is not going to assert from memory — three M0 defects came from
confident, wrong claims about tool behaviour, and CLAUDE.md's first hard-won
rule exists because of them.

So: instrument, run the stack, `curl otel-collector:8889/metrics`, and read the
names that actually come out. The three names in `alerts.yml` are the target. If
the exporter cannot be made to produce `overpass_allocation_duration_ms_bucket`
— for instance if a `ms` unit forces `_milliseconds` — then the alert rule
changes to the real name and this section records what the exporter did.

This is the one part of the design that is a hypothesis rather than a decision.

## Decision 5 — alerts, corrected and split

The four existing rules stay. Two changes:

`TleStale` currently fires at 48h while its own description explains the
behaviour at 72h — "feasibility-service refuses to compute against a stale
element set". `StalenessPolicy` in `element_set.py` puts `fresh` below 24h and
`stale` at or above 72h. The rule and its description disagree, so one of them
is wrong.

Split, because the two thresholds have different remedies:

- `TleAgeing` at 48h, `severity: warning` — nothing is failing yet; a refresh
  is due before it does.
- `TleStale` at 72h, `severity: critical` — feasibility is *now* refusing
  requests for this satellite with `TLE_STALE`. Customer-visible.

Both gain `by (satellite_id)`, for the same reason `DeadLetterQueueNotEmpty` is
per stream: which one is the first thing an operator needs, and the label is
already there.

No new alert families. The issue asks for DLQ depth, outbox lag, and TLE
staleness; all three exist and the work is making them able to fire.

## Decision 6 — two dashboards

`deploy/grafana/dashboards/` is provisioned already and empty.

**`overpass-red.json` — "Overpass — Service Health".** HTTP row (request rate
by route, error ratio, p50/p95/p99), consumer row (rate by subject and outcome,
p95 handle time, redeliveries), delivery row (outbox pending seconds per
service, DLQ depth per stream from `jetstream_stream_total_messages`).

**`overpass-domain.json` — "Overpass — Domain".** Unfulfilled by reason as the
headline, stacked over time and as a table; fulfilled-versus-unfulfilled ratio;
opportunities per request; plan value per round; allocation latency p50/p95/p99
against the 800ms SLO line the `PlannerRoundsSlow` alert already asserts;
satellite utilisation as a bar gauge per satellite; TLE age per satellite with
48h and 72h threshold bands; feasibility refusals by reason.

Two, not three. A third "pipeline flow" dashboard was considered and dropped —
the conservation property it would show is already a contract test, and a
dashboard that restates a test nobody doubts is a panel nobody opens.

Fixed `uid` on both, datasource referenced by the `prometheus` uid that
`datasources.yml` already pins, so a re-provision does not orphan the panels.

## Verification

Two gates, and neither is "the JSON looks right". Every M0 defect compiled,
passed `go vet`, and looked idiomatic.

**`make alerts-test` — promtool rule unit tests.**
`deploy/prometheus/rules/alerts_test.yml`, run through the `prom/prometheus`
image so no new host dependency appears. Every rule gets two cases: **firing on
the series it exists to catch, and silent on the series it must not**. All four
rules are currently unproven and three cannot fire at all, so this gate starts
by failing on the code as committed today — which is the demonstration
CONTRIBUTING.md asks for, obtained for free rather than staged.

**`tests/integration/metrics_test.go` — the dashboard query gate.** Parses
every `expr` out of both committed dashboard JSON files, queries Prometheus
`/api/v1/query` after seeded traffic, and asserts each returns a non-empty
result.

This is the check that "dashboards populate from `make demo` with no manual
setup" actually means. A panel whose query names a metric nobody publishes
renders as "No data" and is invisible to every form of review except opening
it — which is precisely how `overpass_dlq_depth` survived. Extracting the
queries from the committed JSON rather than restating them in the test is the
whole point: a test with its own copy of the query proves the test's query
works.

To watch it fail: rename one metric in one panel and confirm the gate names
that panel.

**Unit level, both languages.** Go instrument tests use
`sdkmetric.NewManualReader`; Python uses `InMemoryMetricReader`. Both assert
instrument names, label sets, and that an outcome label is populated on every
path — deterministic, no collector, no stack. Reading is not verification for
generated code, and an instrument that is never recorded looks identical to one
that is.

## What lands where

| Path | Change |
| --- | --- |
| `lib/go/telemetry/` | New module: tracer + meter provider setup |
| `lib/go/consume/` | OTel instruments behind the existing `Metrics` API |
| `services/tasking-api/` | Migrate onto `lib/go/telemetry`; outbox instruments |
| `services/planner/` | Meter provider; domain instruments; the `main.go:208` discard becomes an export |
| `services/plan-gateway/` | Meter provider; `otelhttp`; consumer instruments |
| `services/feasibility/` | Meter provider; opportunities, refusals, TLE age, outbox |
| `deploy/prometheus/prometheus.yml` | Delete the `overpass-services` job |
| `deploy/prometheus/rules/alerts.yml` | Split `TleStale`; add `satellite_id` |
| `deploy/prometheus/rules/alerts_test.yml` | New: promtool unit tests |
| `deploy/grafana/dashboards/` | Two dashboards |
| `tests/integration/metrics_test.go` | New: dashboard query gate |
| `Makefile`, `.github/` | `make alerts-test`, CI job |
| `docs/decisions/0018-*.md` | Transport, naming, cardinality budget |

This touches all four service tracks and `docs/decisions/`, so the PR carries
`crosses-tracks`. That is the sanctioned hatch — ADR-0013 wants crossings
*visible*, not impossible, and M1-18/19/20 set the precedent for integration
work that genuinely spans every service.

## Out of scope

- The breaker-state gauge from #51. It belongs to that issue; this one makes
  the export path it needs exist.
- Span metrics and the Tempo service graph. `datasources.yml` declares
  `tracesToMetrics` "with M3-06", but that link needs the collector's
  `spanmetrics` connector, which is a tracing-pipeline change and belongs with
  #52.
- Exemplars linking histograms to traces. Genuinely valuable and genuinely a
  separate change; it needs both #52's end-to-end trace and a Grafana
  `exemplarTraceIdDestinations` config, and doing it here would mean debugging
  two new things at once.
