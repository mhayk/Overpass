"""OpenTelemetry metrics for the feasibility service.

The Python half of #53. Metrics take the same path as traces — pushed over
OTLP to the collector, not scraped from a ``/metrics`` endpoint — and for this
service that is not a stylistic choice: it is a worker with no HTTP listener,
deliberately, and ``docker-compose.yml`` already refused to invent one for a
readiness probe. ADR-0018 has the argument.

NO NEW DEPENDENCY. Checked rather than assumed: the metrics SDK ships in
``opentelemetry-sdk`` and the OTLP metric exporter ships in
``opentelemetry-exporter-otlp-proto-grpc``, both of which this service already
had for tracing.

THE INSTRUMENT NAMES CARRY THEIR UNITS AND DECLARE NO ``unit``. Measured
against the running collector: ``overpass.tle.age_hours`` with an empty unit
exports as ``overpass_tle_age_hours``, which is what
``deploy/prometheus/rules/alerts.yml`` has queried since before anything
published it. Declaring the unit properly is what breaks it — a millisecond
instrument with ``unit="ms"`` becomes ``..._milliseconds`` and silently
orphans its alert. One rule, applied everywhere, with no exceptions to
remember.
"""

from __future__ import annotations

import logging
import os
import threading
from typing import TYPE_CHECKING

from opentelemetry import metrics as otel_metrics
from opentelemetry.exporter.otlp.proto.grpc.metric_exporter import OTLPMetricExporter
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.metrics.view import ExplicitBucketHistogramAggregation, View
from opentelemetry.sdk.resources import Resource

if TYPE_CHECKING:
    from collections.abc import Iterable

    from opentelemetry.metrics import CallbackOptions, Meter, Observation

log = logging.getLogger(__name__)

SCOPE_NAME = "feasibility-service"

# Ten seconds, matching prometheus.yml's scrape_interval and the Go services'
# reader. Faster exports samples Prometheus never reads; slower means one
# scrape sees the same cumulative value twice, which flattens a rate.
_EXPORT_INTERVAL_MS = 10_000

# Instrument names. Constants because the dashboards query them and a rename is
# otherwise a silent "No data" panel that nothing else notices.
OPPORTUNITIES_PER_REQUEST = "overpass.opportunities.per_request"
REFUSALS = "overpass.feasibility.refusals"
TLE_AGE_HOURS = "overpass.tle.age_hours"
CONSUME_DURATION_MS = "overpass.consume.duration_ms"
CONSUME_REDELIVERIES = "overpass.consume.redeliveries"
OUTBOX_PENDING_SECONDS = "overpass.outbox.pending_seconds"
OUTBOX_PUBLISHED = "overpass.outbox.published"

# The outcome vocabulary, identical to lib/go/consume's. Two languages
# reporting the same pipeline must agree on these strings or the consumer
# panels need one query per language.
OUTCOME_PROCESSED = "processed"
OUTCOME_DUPLICATE = "duplicate"
OUTCOME_TERMINATED = "terminated"
OUTCOME_DEADLETTERED = "deadlettered"
OUTCOME_FAILED = "failed"

# Millisecond buckets, spanning a fast fold to a delivery slow enough that the
# broker is about to redeliver it. Declared because the SDK's defaults top out
# at 10 in the instrument's own units, which for a millisecond instrument puts
# every real observation in the first bucket and makes p95 read a constant.
_MS_BUCKETS = (1.0, 5.0, 10.0, 25.0, 50.0, 100.0, 250.0, 500.0, 1000.0, 2500.0, 5000.0, 10000.0)

# A sweep returning hundreds of opportunities is a real and interesting case —
# a wide-open request over a nine-satellite constellation — so the top bucket
# is generous.
_OPPORTUNITY_BUCKETS = (0.0, 1.0, 2.0, 5.0, 10.0, 25.0, 50.0, 100.0, 250.0, 500.0)


class Instruments:
    """Everything this service reports, and the state the gauges read from.

    Observable gauges rather than synchronous ones for TLE age and outbox lag,
    because both are last-value facts sampled far more often than they are
    exported. A synchronous gauge would report whichever sweep or batch
    happened to land inside the export window and silently drop the rest.
    """

    def __init__(self, meter: Meter) -> None:
        self._opportunities = meter.create_histogram(
            OPPORTUNITIES_PER_REQUEST,
            description="Opportunities found for one tasking request.",
        )
        self._refusals = meter.create_counter(
            REFUSALS,
            description=(
                "Requests refused before producing an opportunity, by contract reason code."
            ),
        )
        self._consume_duration = meter.create_histogram(
            CONSUME_DURATION_MS,
            description=(
                "Receive-to-outcome latency for one delivery, in milliseconds, "
                "by subject and outcome."
            ),
        )
        self._redeliveries = meter.create_counter(
            CONSUME_REDELIVERIES,
            description="Deliveries the broker had already attempted.",
        )
        self._outbox_published = meter.create_counter(
            OUTBOX_PUBLISHED,
            description="Outbox rows the relay attempted, by outcome.",
        )

        # Guarded because the sweeper, the worker and the relay all run as
        # separate asyncio tasks, and the gauge callbacks are invoked from the
        # SDK's exporter thread.
        self._lock = threading.Lock()
        self._tle_ages: dict[str, float] = {}
        self._outbox_pending: float | None = None

        meter.create_observable_gauge(
            TLE_AGE_HOURS,
            callbacks=[self._observe_tle_ages],
            description="Age of the newest element set held for each satellite, in hours.",
        )
        meter.create_observable_gauge(
            OUTBOX_PENDING_SECONDS,
            callbacks=[self._observe_outbox_pending],
            description="Age of the oldest unpublished outbox row, in seconds.",
        )

    def _observe_tle_ages(self, _options: CallbackOptions) -> Iterable[Observation]:
        from opentelemetry.metrics import Observation as Obs

        with self._lock:
            ages = dict(self._tle_ages)
        return [Obs(age, {"satellite_id": sat}) for sat, age in ages.items()]

    def _observe_outbox_pending(self, _options: CallbackOptions) -> Iterable[Observation]:
        from opentelemetry.metrics import Observation as Obs

        with self._lock:
            pending = self._outbox_pending
        # Nothing before the first batch. Zero lag and "this relay has never
        # drained" are different facts, and reporting the second as the first
        # shows a healthy outbox for a relay that never ran.
        return [] if pending is None else [Obs(pending)]

    def record_opportunities(self, count: int) -> None:
        """Record how many opportunities one request produced."""
        self._opportunities.record(count)

    def record_refusal(self, reason_code: str) -> None:
        """Count one refusal, by the contract reason code that caused it.

        The ingress-side sibling of the planner's requests-unfulfilled-by-reason.
        A request that never produced an opportunity never reaches a round and
        never becomes an unfulfilment, so without this the two counts do not
        reconcile and requests appear to vanish between the services.
        """
        self._refusals.add(1, {"reason": reason_code})

    def record_consume(self, subject: str, outcome: str, duration_ms: float) -> None:
        """Record one finished delivery. Called on every exit path."""
        self._consume_duration.record(duration_ms, {"subject": subject, "outcome": outcome})

    def record_redelivery(self, subject: str) -> None:
        """Count a delivery the broker had tried before."""
        self._redeliveries.add(1, {"subject": subject})

    def record_outbox_batch(self, published: int, failed: int, pending_seconds: float) -> None:
        """Record one drained outbox batch, including an empty one.

        An empty batch reports zero lag rather than nothing: nothing waiting IS
        zero lag, and skipping it would leave the gauge pinned at the last
        non-empty measurement so OutboxRelayLagging could never stop firing.
        """
        if published:
            self._outbox_published.add(published, {"outcome": "published"})
        if failed:
            self._outbox_published.add(failed, {"outcome": "failed"})
        with self._lock:
            self._outbox_pending = pending_seconds

    def set_tle_age(self, satellite_id: str, age_hours: float) -> None:
        """Record the age of the element set currently held for a satellite.

        Per satellite rather than a histogram. With nine satellites the labelled
        gauges ARE the distribution, and they answer the question a histogram
        cannot: WHICH element set is old, which is the first thing an operator
        needs before a refresh.
        """
        with self._lock:
            self._tle_ages[satellite_id] = age_hours


class _NoOpInstruments(Instruments):
    """What every call site gets until setup() runs.

    Subclasses rather than duck-types so mypy sees one type. Every method is
    replaced with a no-op: telemetry must not become a correctness dependency,
    and the tests, the benchmarks and any script importing the pipeline all run
    without a meter.
    """

    def __init__(self) -> None:
        pass

    def record_opportunities(self, count: int) -> None:
        pass

    def record_refusal(self, reason_code: str) -> None:
        pass

    def record_consume(self, subject: str, outcome: str, duration_ms: float) -> None:
        pass

    def record_redelivery(self, subject: str) -> None:
        pass

    def record_outbox_batch(self, published: int, failed: int, pending_seconds: float) -> None:
        pass

    def set_tle_age(self, satellite_id: str, age_hours: float) -> None:
        pass


_instruments: Instruments = _NoOpInstruments()


def instruments() -> Instruments:
    """The process-wide instruments. A no-op until setup() installs real ones."""
    return _instruments


def build(meter: Meter) -> Instruments:
    """Build instruments against an arbitrary meter. For tests."""
    return Instruments(meter)


def views() -> list[View]:
    """Histogram bucket boundaries, which the SDK only accepts through views."""
    return [
        View(
            instrument_name=CONSUME_DURATION_MS,
            aggregation=ExplicitBucketHistogramAggregation(_MS_BUCKETS),
        ),
        View(
            instrument_name=OPPORTUNITIES_PER_REQUEST,
            aggregation=ExplicitBucketHistogramAggregation(_OPPORTUNITY_BUCKETS),
        ),
    ]


def setup(
    *,
    service_name: str = "feasibility-service",
    service_version: str | None = None,
    environment: str | None = None,
    endpoint: str | None = None,
) -> MeterProvider | None:
    """Install a global meter provider. Returns None when export is off.

    An unreachable collector is NOT a startup failure, for the same reason
    tracing's setup says so: refusing to compute access windows because a
    metrics backend is down would make observability an availability
    dependency, which is exactly backwards.
    """
    global _instruments

    endpoint = endpoint if endpoint is not None else os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
    if not endpoint:
        return None

    resource = Resource.create(
        {
            "service.name": service_name,
            # `os.getenv(k) or default`, not `os.getenv(k, default)`. The
            # two-argument form is what you would write first and mypy strict
            # rejects it here — it widens the dict's value type to include
            # None. telemetry.py already settled on this spelling; matching it
            # keeps one resource shape across both signals.
            "service.version": service_version or os.getenv("OVERPASS_VERSION") or "dev",
            "deployment.environment": (environment or os.getenv("OVERPASS_ENV") or "development"),
        }
    )

    reader = PeriodicExportingMetricReader(
        # Plaintext. The collector is on the compose network; TLS between two
        # containers on the same bridge buys nothing and adds certificates to a
        # stack whose whole point is starting in one command.
        OTLPMetricExporter(endpoint=endpoint, insecure=True),
        export_interval_millis=_EXPORT_INTERVAL_MS,
    )
    provider = MeterProvider(resource=resource, metric_readers=[reader], views=views())
    otel_metrics.set_meter_provider(provider)

    _instruments = Instruments(provider.get_meter(SCOPE_NAME))
    log.info("metrics exporting to %s", endpoint)
    return provider
