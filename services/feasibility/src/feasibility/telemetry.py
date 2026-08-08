"""OpenTelemetry tracing for the feasibility service.

One export target: the collector. Not Tempo directly — a service should know
one endpoint, so sampling, redaction or a second backend has one place to live.

The consuming half of the trace that starts in tasking-api. What arrives on a
NATS message is a W3C ``traceparent`` naming the outbox publish span; extracting
it is what turns "this service produced some spans" into "this service's work
appears under the HTTP request that asked for it".
"""

from __future__ import annotations

import logging
import os
from typing import TYPE_CHECKING, Any

from opentelemetry import trace
from opentelemetry.baggage.propagation import W3CBaggagePropagator
from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
from opentelemetry.propagate import extract, inject, set_global_textmap
from opentelemetry.propagators.composite import CompositePropagator
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.sdk.trace.sampling import ParentBased, TraceIdRatioBased
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

if TYPE_CHECKING:
    from collections.abc import Mapping
    from contextlib import AbstractContextManager

log = logging.getLogger(__name__)

SCOPE_NAME = "feasibility-service"

# Half a second, matching tasking-api. The default is five, which is longer than
# the end-to-end test will wait and longer than a demo's patience for a trace to
# appear.
_EXPORT_INTERVAL_MS = 500


def setup(
    *,
    service_name: str = "feasibility-service",
    service_version: str | None = None,
    environment: str | None = None,
    endpoint: str | None = None,
    sample_ratio: float | None = None,
) -> TracerProvider | None:
    """Install a global tracer provider. Returns None when tracing is off.

    An unreachable collector is NOT a startup failure. Tracing is observability,
    not function; refusing to consume messages because a telemetry backend is
    down would make the observability stack an availability dependency, which is
    exactly backwards. The exporter retries in the background and drops spans in
    the meantime.
    """
    # The propagator is installed even when export is off. It costs nothing, and
    # it means a traceparent still travels THROUGH this service to the next one
    # — so a partially instrumented deployment produces one trace with a gap
    # rather than two unrelated traces.
    #
    # In Python this is belt-and-braces, and that is worth stating because the
    # Go side is not. Measured: opentelemetry-python's DEFAULT global propagator
    # is already a CompositePropagator over traceparent, tracestate and baggage,
    # whereas Go's default is a no-op and the equivalent call there is
    # load-bearing. Setting it here is still right — OTEL_PROPAGATORS can
    # replace the default, and a future release could change it — but no test
    # can distinguish this line from its absence, so none pretends to.
    #
    # In Python this is belt-and-braces, and that is worth stating because the
    # Go side is not. Measured: opentelemetry-python's DEFAULT global propagator
    # is already a CompositePropagator over traceparent, tracestate and baggage,
    # whereas Go's default is a no-op and the equivalent call there is
    # load-bearing. Setting it here is still right — OTEL_PROPAGATORS can
    # replace the default, and a future release could change it — but no test
    # can distinguish this line from its absence, so no test pretends to.
    set_global_textmap(
        CompositePropagator([TraceContextTextMapPropagator(), W3CBaggagePropagator()])
    )

    endpoint = (
        endpoint
        if endpoint is not None
        else os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "otel-collector:4317")
    )
    if not endpoint:
        return None

    if sample_ratio is None:
        sample_ratio = float(os.getenv("TRACE_SAMPLE_RATIO", "1.0"))
    if not 0.0 <= sample_ratio <= 1.0:
        # Rejected rather than clamped. A value outside the range means the
        # author believed something about this knob that is not true, and
        # quietly treating 2.0 as 1.0 leaves the belief in place.
        message = f"TRACE_SAMPLE_RATIO must be between 0 and 1, got {sample_ratio}"
        raise ValueError(message)

    resource = Resource.create(
        {
            "service.name": service_name,
            "service.version": service_version or os.getenv("OVERPASS_VERSION") or "dev",
            "deployment.environment": (environment or os.getenv("OVERPASS_ENV") or "development"),
        }
    )
    provider = TracerProvider(
        resource=resource,
        # ParentBased, so a decision made upstream is honoured rather than
        # re-rolled. Re-rolling per service is how a trace gets holes in it.
        sampler=ParentBased(TraceIdRatioBased(sample_ratio)),
    )
    provider.add_span_processor(
        BatchSpanProcessor(
            # Plaintext: the collector is on the compose network, and TLS
            # between two containers on one bridge buys nothing while adding
            # certificates to a stack whose point is starting in one command.
            OTLPSpanExporter(endpoint=endpoint, insecure=True),
            schedule_delay_millis=_EXPORT_INTERVAL_MS,
        )
    )
    trace.set_tracer_provider(provider)
    return provider


def tracer() -> trace.Tracer:
    """This service's tracer."""
    return trace.get_tracer(SCOPE_NAME)


def consumer_span(
    name: str,
    headers: Mapping[str, str] | None,
    attributes: dict[str, Any] | None = None,
) -> AbstractContextManager[trace.Span]:
    """Start a consumer span that is both a CHILD and a LINK of the producer.

    Both, deliberately, and the reason is the shape of an async hop.

    A pure parent-child relationship says the consumer ran *inside* the
    producer, which is how a synchronous call looks — the publish would appear
    to contain the consume, and the producer's duration would swallow however
    long the message sat in the queue. A pure link says the two are related and
    loses the causal chain: the consumer becomes a root, and asking "what
    happened to this request" returns two traces that a human has to join.

    Child gives the chain. Link states, in the model rather than in a comment,
    that this is a message-driven continuation and not a nested call. Tempo
    renders both, and a reader can see at a glance that the gap between the
    spans is queue time rather than work.

    A message with no traceparent yields a root span rather than an error. That
    is the correct answer for an event published before this instrumentation
    existed, or by a producer that has none.
    """
    parent_context = extract(dict(headers or {}))
    producer = trace.get_current_span(parent_context).get_span_context()

    links = [trace.Link(producer)] if producer.is_valid else []
    return tracer().start_as_current_span(
        name,
        context=parent_context,
        kind=trace.SpanKind.CONSUMER,
        links=links,
        attributes=attributes or {},
    )


def trace_headers(headers: Mapping[str, str] | None = None) -> dict[str, str]:
    """Inject the current span into a header map for the next hop.

    Used by the outbox: an event this service publishes carries the span that
    produced it, exactly as tasking-api's does, so the chain continues past the
    second service instead of ending at it.
    """
    carrier: dict[str, str] = dict(headers or {})
    inject(carrier)
    return carrier


def current_ids() -> tuple[str, str]:
    """Trace and span ids as hex, or empty strings.

    Empty rather than the all-zero id. ``00000000000000000000000000000000`` in a
    log line looks like a real id and matches nothing, which sends whoever reads
    it looking for a trace that cannot exist.
    """
    span_context = trace.get_current_span().get_span_context()
    if not span_context.is_valid:
        return "", ""
    return format(span_context.trace_id, "032x"), format(span_context.span_id, "016x")


class TraceContextFilter(logging.Filter):
    """Adds trace_id and span_id to every record.

    A filter rather than a formatter change, so it applies wherever the record
    is emitted and whatever format is configured. Empty strings when there is no
    span, so the fields always exist and a log pipeline never has to cope with a
    key that sometimes is not there.
    """

    def filter(self, record: logging.LogRecord) -> bool:
        record.trace_id, record.span_id = current_ids()
        return True
