"""The consuming half of the trace.

Every assertion here is about a property that fails silently. A service with
broken propagation still produces spans and still exports them — it just
produces a forest of one-span traces, which looks like working tracing until
somebody asks what happened to a particular request.
"""

from __future__ import annotations

import logging
from collections.abc import Iterator
from typing import Any, cast

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from feasibility import telemetry

# A fixed, valid W3C traceparent. Sampled flag set, so the sampler keeps it.
UPSTREAM_TRACE_ID = "4bf92f3577b34da6a3ce929d0e0e4736"
UPSTREAM_SPAN_ID = "00f067aa0ba902b7"
UPSTREAM = {"traceparent": f"00-{UPSTREAM_TRACE_ID}-{UPSTREAM_SPAN_ID}-01"}


@pytest.fixture
def spans() -> Iterator[InMemorySpanExporter]:
    """A real provider exporting into memory.

    Not a mock. The behaviour under test is what the SDK does with a context —
    parenting, linking, sampling — and a mock would only confirm that this file
    and the test agree with each other.
    """
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))

    previous = trace.get_tracer_provider()
    # setup() with an empty endpoint installs the propagator and no exporter,
    # which is exactly what a test wants: real propagation, no network.
    telemetry.setup(endpoint="")
    trace._TRACER_PROVIDER = provider
    yield exporter
    trace._TRACER_PROVIDER = previous


def test_a_consumer_span_joins_the_producers_trace(spans: InMemorySpanExporter) -> None:
    """The whole point of #32: one trace, not two."""
    with telemetry.consumer_span("tasking.request.received.v1 process", UPSTREAM):
        pass

    (span,) = spans.get_finished_spans()
    assert format(span.context.trace_id, "032x") == UPSTREAM_TRACE_ID, (
        "the consumer started its own trace; the request and its processing "
        "would appear as two unrelated traces"
    )
    assert span.parent is not None
    assert format(span.parent.span_id, "016x") == UPSTREAM_SPAN_ID


def test_the_consumer_span_is_both_a_child_and_a_link(spans: InMemorySpanExporter) -> None:
    """Both, deliberately, and each carries information the other does not.

    A pure child says the consumer ran INSIDE the producer, which is how a
    synchronous call looks — the publish would appear to contain the consume and
    swallow however long the message waited in the queue. A pure link keeps the
    two separate and loses the chain: the consumer becomes a root, and "what
    happened to this request" returns two traces a human has to join.
    """
    with telemetry.consumer_span("tasking.request.received.v1 process", UPSTREAM):
        pass

    (span,) = spans.get_finished_spans()

    assert span.parent is not None, "no parent: the causal chain is broken"
    assert span.links, "no link: nothing marks this as a message-driven continuation"

    (link,) = span.links
    assert format(link.context.span_id, "016x") == UPSTREAM_SPAN_ID
    assert format(link.context.trace_id, "032x") == UPSTREAM_TRACE_ID


def test_the_span_kind_is_consumer(spans: InMemorySpanExporter) -> None:
    """Kind drives how Tempo renders the hop and how span metrics classify it.

    INTERNAL would make a message-driven continuation look like ordinary work,
    and the service graph in M3-06 reads exactly this attribute to know there is
    a queue between two services.
    """
    with telemetry.consumer_span("subject process", UPSTREAM):
        pass

    (span,) = spans.get_finished_spans()
    assert span.kind is trace.SpanKind.CONSUMER


def test_a_message_with_no_traceparent_still_produces_a_span(
    spans: InMemorySpanExporter,
) -> None:
    """A root span, not an error.

    Correct for an event published before this instrumentation existed, or by a
    producer that has none. Refusing to process it would make a telemetry gap
    into a delivery failure.
    """
    with telemetry.consumer_span("subject process", {}):
        pass

    (span,) = spans.get_finished_spans()
    assert span.parent is None
    assert not span.links, "linked to a producer that was never named"
    assert span.context.trace_id != 0


def test_a_malformed_traceparent_is_ignored_rather_than_fatal(
    spans: InMemorySpanExporter,
) -> None:
    """A corrupt header must not take the consumer down.

    The W3C spec says an unparseable traceparent is treated as absent, and that
    is the right behaviour here too: the alternative is one bad publisher
    stalling a stream.
    """
    with telemetry.consumer_span("subject process", {"traceparent": "nonsense"}):
        pass

    (span,) = spans.get_finished_spans()
    assert span.parent is None
    assert span.context.trace_id != 0


def test_attributes_reach_the_span(spans: InMemorySpanExporter) -> None:
    with telemetry.consumer_span(
        "subject process",
        UPSTREAM,
        {"messaging.system": "nats", "messaging.message.id": "evt-1"},
    ):
        pass

    (span,) = spans.get_finished_spans()
    attributes = span.attributes or {}
    assert attributes["messaging.system"] == "nats"
    assert attributes["messaging.message.id"] == "evt-1"


def test_the_chain_continues_past_this_service(spans: InMemorySpanExporter) -> None:
    """An event this service publishes carries ITS span, not the one it received.

    Same rule as tasking-api's relay, for the same reason: the next consumer
    should hang off the work that produced its message, not off a grandparent.
    """
    with telemetry.consumer_span("subject process", UPSTREAM):
        outgoing = telemetry.trace_headers()

    (span,) = spans.get_finished_spans()
    assert UPSTREAM_TRACE_ID in outgoing["traceparent"], "left the trace"
    assert format(span.context.span_id, "016x") in outgoing["traceparent"], (
        "forwarded the received traceparent verbatim; the next hop would be a "
        "sibling of this work rather than a consequence of it"
    )


def test_propagation_works_with_tracing_disabled() -> None:
    """Why setup() installs the propagator before checking the endpoint.

    A service with no exporter must still pass a traceparent through, so a
    partially instrumented deployment yields one trace with a gap rather than
    two unrelated traces.
    """
    assert telemetry.setup(endpoint="") is None

    context = telemetry.consumer_span
    assert context is not None  # the helper is usable with no provider

    from opentelemetry.propagate import extract, inject

    carrier: dict[str, str] = {}
    token = extract(UPSTREAM)
    from opentelemetry import context as otel_context

    restore = otel_context.attach(token)
    try:
        inject(carrier)
    finally:
        otel_context.detach(restore)

    assert UPSTREAM_TRACE_ID in carrier.get("traceparent", ""), (
        "the trace was not forwarded through a service with no exporter"
    )


def test_current_ids_are_empty_rather_than_zero(spans: InMemorySpanExporter) -> None:
    """The all-zero id in a log line looks real and matches nothing.

    Whoever reads it goes to Tempo and searches for a trace that cannot exist,
    which is a worse experience than an absent field.
    """
    trace_id, span_id = telemetry.current_ids()
    assert (trace_id, span_id) == ("", "")

    with telemetry.consumer_span("subject process", UPSTREAM):
        trace_id, span_id = telemetry.current_ids()
    assert trace_id == UPSTREAM_TRACE_ID
    assert len(span_id) == 16


def test_the_log_filter_always_supplies_both_fields(spans: InMemorySpanExporter) -> None:
    """Present even with no span, so a log pipeline never meets a missing key."""
    record = logging.LogRecord("t", logging.INFO, "f", 1, "m", None, None)
    assert telemetry.TraceContextFilter().filter(record)
    assert cast("Any", record).trace_id == ""
    assert cast("Any", record).span_id == ""

    with telemetry.consumer_span("subject process", UPSTREAM):
        record = logging.LogRecord("t", logging.INFO, "f", 1, "m", None, None)
        telemetry.TraceContextFilter().filter(record)
    assert cast("Any", record).trace_id == UPSTREAM_TRACE_ID


def test_an_out_of_range_sample_ratio_is_rejected() -> None:
    """Rejected, not clamped.

    A value outside [0,1] means the author believed something about this knob
    that is not true, and quietly treating 2.0 as 1.0 leaves the belief in place
    until it matters.
    """
    with pytest.raises(ValueError, match="between 0 and 1"):
        telemetry.setup(endpoint="otel-collector:4317", sample_ratio=2.0)
    with pytest.raises(ValueError, match="between 0 and 1"):
        telemetry.setup(endpoint="otel-collector:4317", sample_ratio=-0.5)
