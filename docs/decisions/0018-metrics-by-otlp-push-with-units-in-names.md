# 0018 — Push metrics over OTLP, and bake units into instrument names

- **Status:** accepted
- **Date:** 2026-08-10
- **Deciders:** Mhayk Whandson
- **Supersedes:** nothing. It corrects an assumption in
  `docs/superpowers/specs/2026-08-09-breaker-and-bulkhead-design.md`, which
  said the breaker gauge "needs the `/metrics` surface #53 brings". There is no
  `/metrics` surface.

## Context and problem statement

M3-06 needed every service to report RED and domain metrics. The scaffolding
around that was already in place and had never carried a single sample, which
made the decision less about what to build than about what to believe:

- The collector already had a working metrics pipeline — OTLP in, Prometheus
  exposition on `:8889`, `resource_to_telemetry_conversion` enabled.
- `prometheus.yml` ALSO had a job scraping `/metrics` on all four services. No
  service served that path. Two of the four ports in it were wrong. One of the
  four services has no HTTP listener at all, on purpose.
- Three of the four alert rules named metrics nothing published, so they could
  not fire.

So the questions were: **how do metrics get from a process to Prometheus in a
system where one service is a worker with no listener — and what exactly do the
instruments have to be called for the alert rules that already exist to work?**

The second question sounds like a detail. It is the one that cost the most to
get right, and the only one where the obvious answer is wrong.

## Decision drivers

- **One instrumentation path across two languages.** Go and Python must produce
  the same label shape, or every dashboard panel needs one query per language.
- **feasibility has no HTTP listener, deliberately.** `docker-compose.yml`
  refuses to invent one even for a readiness probe. Whatever is decided must
  not reverse that for the sake of a scraper.
- **The three metric names in `alerts.yml` are already committed.** Rules
  written before their metrics existed are still rules, and changing them to
  match whatever the exporter happened to produce would be letting the tail wag
  the dog.
- **Label cardinality is a production risk, not a style question.** The read
  API's routes contain a satellite id and a timestamp.
- Telemetry must never become an availability dependency. Established by
  `telemetry.Setup` and not up for renegotiation here.

## Considered options

1. **OTLP push only** — services push to the collector; Prometheus scrapes only
   the collector.
2. **Per-service `/metrics`, scraped** — the conventional Prometheus shape.
3. **Both** — push for domain metrics, a thin `/metrics` for `up{}`.

## Decision outcome

Chosen: **Option 1, OTLP push only**, and the `overpass-services` scrape job is
deleted rather than left to fail.

Option 2 means inventing an HTTP server inside the Python worker purely to be
scraped. The compose file already declined to do that for a readiness probe,
and the argument is not weaker for metrics than it was for health: a listener
that exists only to be polled is a surface with no user. It also means a second
metrics stack living beside the OTel SDK, with two naming conventions and two
places to configure a label.

Option 3 doubles the export path for the same numbers, which is how a metric
ends up counted twice and how a configuration stops explaining itself.

**Deleting the dead job was not optional.** Four targets permanently `down` is
not a partial signal, it is a false one, and the first person to open
Prometheus reads it as an outage. It was also actively polluting the RED
metrics it appeared to support: every scrape of a path that does not exist is a
404 recorded on the HTTP histogram. Two of its four ports were wrong as well —
`plan-gateway` listens on 8083, not 8081 — which nobody had noticed, because a
target that is down for the right reason and a target that is down for the
wrong reason look identical.

### What this costs

**There is no per-service `up{}` series.** A dead service stops sending, its
gauges go stale after five minutes, and its counters simply stop advancing.
This is a real loss and it is stated here rather than buried, because it is the
one thing Option 2 was better at.

Liveness detection stays where it already was: compose healthchecks on the
three HTTP services, and restart counts plus logs on the worker. That is the
same place it lived before this ADR, so nothing regressed — but a future
deployment target with no healthchecks would need to solve it again.

## The naming rule, and why it was measured

**Bake the unit into the instrument name. Leave the OTel `unit` field EMPTY.**

```
overpass.allocation.duration_ms   unit ""    → overpass_allocation_duration_ms_bucket
overpass.allocation.duration      unit "ms"  → overpass_allocation_duration_milliseconds_bucket
overpass.tle.age_hours            unit ""    → overpass_tle_age_hours
overpass.requests.unfulfilled     unit ""    → overpass_requests_unfulfilled_total
```

The first form produces exactly the names `alerts.yml` has queried since before
anything published them. **The second form — declaring the unit properly, which
is what a careful engineer writes first — renames every series and silently
orphans the alert.** The rule fires on nothing, forever, and looks fine.

This was measured, not reasoned about, by pushing candidate instruments to the
running collector as raw OTLP/HTTP JSON — no SDK, so nothing sat between the
payload and the exporter's translation — and reading `:8889/metrics` back.
CLAUDE.md's first hard-won rule exists because three M0 defects came from
confident, wrong claims about tool behaviour, and this is exactly that shape of
claim.

The semconv HTTP instrument is the one exception: `otelhttp` declares
`unit: "s"` and the resulting `http_server_request_duration_seconds` is the
name every other Prometheus deployment already uses. Adopting an idiosyncratic
name to satisfy an internal rule would be worse.

**Consequence:** a well-meaning "cleanup" that adds proper units to these
instruments breaks the alerts without breaking a test — so tests in Go and
Python assert the empty unit directly, and say why in their names.

## Labels come from the resource

`resource_to_telemetry_conversion` was already enabled, so every series arrives
with `service_name`, `service_version`, `deployment_environment` and `job`
without any instrument declaring them. **No instrument carries an explicit
`service` label.** An earlier draft did, which would have produced `service`
and `service_name` side by side, free to disagree the first time one was set
from a different string.

### Cardinality budget

Every label is a bounded set, and the bound is a property of the domain rather
than a hope:

| Label | Values | Bound |
| --- | --- | --- |
| `reason_code` | 7 | contract enum |
| `satellite_id` | 9 | seeded constellation |
| `policy` | 4 | contract enum |
| `outcome` | 6 | `lib/go/consume` constants |
| `subject` | ~6 | declared stream subjects |
| `http_route` | ~10 | chi route patterns |

**`http_route` is the pattern, never the path.** `/v1/plans/{satellite_id}/
{bucket_start}` is one series; the raw path is one series per satellite per
three-hour bucket, which is an unbounded label set and a Prometheus instance
killed by its own instrumentation.

Getting that right needed work. `otelhttp` wraps the router from the outside —
correct for the span, which must enclose routing — so it never learns chi's
routing decision, and the first working version of these metrics carried **no
route label at all**. Every request in a service collapsed into one series.
Nothing failed; the metric was simply useless, and it took reading the real
exposition to notice. `RouteTag` fixes it through otelhttp's `Labeler`.

`customer_id`, `request_id` and `round_id` are never labels. They are trace
attributes, and they are why traces exist.

## Consequences

- Grafana dashboards are committed JSON, provisioned on startup, and gated:
  `make dashboards-check` extracts every panel expression from the committed
  files and asks Prometheus whether it can render. The queries are never
  restated in the check — a check carrying its own copy of the query proves
  only that the check's query works, which is precisely how `overpass_dlq_depth`
  survived review.
- `make alerts-test` unit-tests every alert rule with promtool, both firing and
  silent. Run against the pre-#53 rules, three cases fail.
- The breaker-state gauge #51 wants rides this path. There is no `/metrics`
  surface for it to land on, and it does not need one.
- Exemplars linking histograms to traces are now cheap to add and deliberately
  not added here. They need #52's end-to-end trace first, and doing both at
  once means debugging two new things simultaneously.
