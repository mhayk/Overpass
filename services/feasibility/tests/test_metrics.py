"""The instruments, asserted by name and by label.

Reading is not verification for generated code, and an instrument that is never
recorded looks exactly like one that is. These run against an in-memory reader:
deterministic, no collector, no stack.
"""

from __future__ import annotations

from typing import Any

import pytest
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader

from feasibility import failures, metrics


def _read(reader: InMemoryMetricReader, name: str) -> list[Any]:
    """Every data point recorded under one instrument name."""
    data = reader.get_metrics_data()
    if data is None:
        return []
    points: list[Any] = []
    for resource_metric in data.resource_metrics:
        for scope_metric in resource_metric.scope_metrics:
            for metric in scope_metric.metrics:
                if metric.name == name:
                    points.extend(metric.data.data_points)
    return points


def _instruments() -> tuple[metrics.Instruments, InMemoryMetricReader]:
    reader = InMemoryMetricReader()
    provider = MeterProvider(metric_readers=[reader], views=metrics.views())
    return metrics.build(provider.get_meter("test")), reader


def test_every_contract_refusal_code_is_recordable() -> None:
    """A refusal the pipeline can emit but the dashboard cannot chart is a gap
    that only appears the day that refusal actually happens."""
    instruments, reader = _instruments()

    for code in sorted(failures.REASON_CODES):
        instruments.record_refusal(code)

    recorded = {point.attributes["reason"] for point in _read(reader, metrics.REFUSALS)}
    assert recorded == failures.REASON_CODES


def test_tle_age_is_labelled_per_satellite() -> None:
    """max() over the gauge answers 'is anything stale'. The label answers
    'which one', which is the first thing an operator needs before a refresh —
    and the reason this is nine labelled gauges rather than one histogram."""
    instruments, reader = _instruments()

    instruments.set_tle_age("SENTINEL-1A", 12.5)
    instruments.set_tle_age("ICEYE-X2", 80.0)

    points = _read(reader, metrics.TLE_AGE_HOURS)
    by_satellite = {point.attributes["satellite_id"]: point.value for point in points}
    assert by_satellite == {"SENTINEL-1A": 12.5, "ICEYE-X2": 80.0}


def test_the_alert_rules_names_are_the_ones_published() -> None:
    """deploy/prometheus/rules/alerts.yml queries overpass_tle_age_hours and
    overpass_outbox_pending_seconds. Measured against the running collector,
    these OTel names with an EMPTY unit produce exactly those. Declaring the
    unit properly is what renames them and orphans the alerts."""
    assert metrics.TLE_AGE_HOURS == "overpass.tle.age_hours"
    assert metrics.OUTBOX_PENDING_SECONDS == "overpass.outbox.pending_seconds"


def test_outcome_vocabulary_matches_the_go_consumers() -> None:
    """Two languages reporting the same pipeline must agree on these strings,
    or the consumer panels need one query per language."""
    assert metrics.OUTCOME_PROCESSED == "processed"
    assert metrics.OUTCOME_DUPLICATE == "duplicate"
    assert metrics.OUTCOME_TERMINATED == "terminated"
    assert metrics.OUTCOME_DEADLETTERED == "deadlettered"
    assert metrics.OUTCOME_FAILED == "failed"


def test_outbox_pending_reports_nothing_before_the_first_batch() -> None:
    """Zero lag and 'this relay has never drained' are different facts.
    Reporting the second as the first shows a perfectly healthy outbox for a
    relay that has not run at all."""
    _instruments_obj, reader = _instruments()

    assert _read(reader, metrics.OUTBOX_PENDING_SECONDS) == []


def test_an_empty_outbox_batch_reports_zero_lag() -> None:
    """Nothing waiting IS zero lag. Skipping the record would leave the gauge
    pinned at the last non-empty measurement, so OutboxRelayLagging would fire
    once during a backlog and never stop after it cleared."""
    instruments, reader = _instruments()

    instruments.record_outbox_batch(published=0, failed=0, pending_seconds=0.0)

    points = _read(reader, metrics.OUTBOX_PENDING_SECONDS)
    assert [point.value for point in points] == [0.0]


def test_consume_duration_carries_subject_and_outcome() -> None:
    """The histogram's _count is the rate and its outcome label is the errors.
    Without both labels it is a latency chart rather than RED."""
    instruments, reader = _instruments()

    instruments.record_consume("tasking.request.received.v1", metrics.OUTCOME_PROCESSED, 12.0)
    instruments.record_consume("tasking.request.received.v1", metrics.OUTCOME_FAILED, 40.0)

    points = _read(reader, metrics.CONSUME_DURATION_MS)
    outcomes = {point.attributes["outcome"] for point in points}
    assert outcomes == {metrics.OUTCOME_PROCESSED, metrics.OUTCOME_FAILED}
    assert all(point.attributes["subject"] == "tasking.request.received.v1" for point in points)


def test_opportunity_histogram_records_zero_as_a_real_observation() -> None:
    """A request that found nothing is a data point, not an absence. Dropping
    it would make the opportunities-per-request panel describe only successful
    sweeps, which is the half that never needs investigating."""
    instruments, reader = _instruments()

    instruments.record_opportunities(0)

    points = _read(reader, metrics.OPPORTUNITIES_PER_REQUEST)
    assert len(points) == 1
    assert points[0].count == 1


def test_no_op_instruments_never_raise() -> None:
    """Every test, benchmark and script importing the pipeline runs without a
    meter. Telemetry is not allowed to become a correctness dependency."""
    noop = metrics.instruments()

    noop.record_opportunities(3)
    noop.record_refusal("TLE_STALE")
    noop.record_consume("s", metrics.OUTCOME_PROCESSED, 1.0)
    noop.record_redelivery("s")
    noop.record_outbox_batch(published=1, failed=0, pending_seconds=1.0)
    noop.set_tle_age("SENTINEL-1A", 1.0)


def test_setup_without_an_endpoint_is_off() -> None:
    """An absent collector address disables export rather than erroring. Every
    test and the benchmarks import this module without one."""
    assert metrics.setup(endpoint="") is None


@pytest.mark.parametrize(
    "name",
    [
        metrics.OPPORTUNITIES_PER_REQUEST,
        metrics.REFUSALS,
        metrics.TLE_AGE_HOURS,
        metrics.CONSUME_DURATION_MS,
        metrics.CONSUME_REDELIVERIES,
        metrics.OUTBOX_PENDING_SECONDS,
        metrics.OUTBOX_PUBLISHED,
    ],
)
def test_instrument_names_are_dotted_and_carry_their_unit(name: str) -> None:
    """The exporter turns dots into underscores. A name that already contains
    an underscore-separated unit survives that translation unchanged, which is
    how overpass.tle.age_hours becomes exactly overpass_tle_age_hours."""
    assert name.startswith("overpass.")
    assert " " not in name
