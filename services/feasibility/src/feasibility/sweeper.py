"""The rolling ephemeris sweep: the one producer here that no message triggers.

Every other publish in this service is a reaction — a tasking request arrives, a
sweep runs, opportunities or a refusal go into the outbox. This one is a
reaction to the clock. The globe needs to know where the satellites are over the
next day, and nobody asks for that; it is simply true, and it stops being true as
time passes.

THE ORDER OF OPERATIONS IS THE DESIGN, and it is the opposite of the obvious one:

    derive every event id  ->  ask which already exist  ->  propagate the rest

The obvious order — propagate everything, then deduplicate on the way into the
outbox — is correct and unaffordable. The id needs only `(satellite_id, bucket,
tle_epoch)`, none of which requires physics; the body needs SGP4 over a thousand
instants. Sweeping a twelve-satellite constellation across a day of three-hour
buckets is around a hundred tracks, and re-propagating all of them every few
minutes to discard all of them is most of a CPU doing nothing.

So the steady state is one SELECT and no arithmetic, and the only tick that does
real work is the one where the horizon has rolled far enough to uncover a bucket.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from typing import Any

import psycopg
from opentelemetry import trace

from feasibility import metrics, telemetry
from feasibility.ephemeris import build_event, derive_event_id
from feasibility.messaging.outbox import already_enqueued, enqueue_once
from feasibility.orbit.ephemeris import SamplingPolicy, bucket_starts, sample_track
from feasibility.orbit.propagation import Propagator
from feasibility.tle.element_set import StalenessPolicy
from feasibility.tle.store import newest_element_sets

log = logging.getLogger(__name__)

# How long to wait before sweeping again when the constellation is not seeded
# yet. Short, because the only thing that resolves it is someone running the
# seeder, and the cost of asking again is one indexed query against an empty
# table.
_UNSEEDED_RETRY_S = 10.0


@dataclass(frozen=True)
class SweeperConfig:
    dsn: str = "postgres://overpass:overpass@localhost:5433/overpass"
    # Every five minutes. The horizon rolls forward continuously and the buckets
    # do not, so what this actually controls is how soon after a bucket boundary
    # the next one appears — not how fresh the data is. Anything comfortably
    # under the bucket length gives the relay time to drain before the globe
    # needs it.
    tick_s: float = 300.0
    # `field(default_factory=...)` rather than a shared instance. SamplingPolicy
    # is frozen so sharing one would be harmless today, and would stop being
    # harmless the moment it gained a mutable field.
    policy: SamplingPolicy = field(default_factory=SamplingPolicy)


@dataclass(frozen=True)
class SweepStats:
    """What one tick did. Returned rather than logged, so tests can assert it."""

    satellites: int = 0
    buckets: int = 0
    tracks_propagated: int = 0
    enqueued: int = 0
    already_published: int = 0


def sweep_once(
    connection: psycopg.Connection[Any],
    now: datetime,
    policy: SamplingPolicy | None = None,
    staleness: StalenessPolicy | None = None,
    headers: dict[str, str] | None = None,
) -> SweepStats:
    """Ensure every satellite's track over the horizon is in the outbox.

    `now` is passed rather than read from the clock, for the same reason
    `evaluate` takes it: a sweep that consults wall time cannot be tested
    against a frozen snapshot, and this one is.

    Writes to the outbox and nothing else. The relay is what puts events on the
    wire — publishing from here would reintroduce the dual write the outbox
    exists to remove.
    """
    policy = policy or SamplingPolicy()
    staleness = staleness or StalenessPolicy()

    entries = newest_element_sets(connection, now)

    # Report every element set's age on every sweep, whether or not anything
    # gets published for it. Per satellite rather than as a distribution: with
    # nine satellites the labelled gauges ARE the distribution, and they answer
    # the question a histogram cannot — WHICH element set is old, which is the
    # first thing an operator needs before ordering a refresh.
    for entry in entries:
        metrics.instruments().set_tle_age(entry.satellite_id, entry.element_set.age_hours(now))

    starts = bucket_starts(now, policy)
    if not entries:
        log.warning("ephemeris sweep found no element sets; is the constellation seeded?")
        return SweepStats(buckets=len(starts))

    # (entry, bucket start, derived id) for the whole horizon, before any
    # physics happens.
    wanted = [
        (entry, start, derive_event_id(entry.satellite_id, start, entry.element_set.epoch))
        for entry in entries
        for start in starts
    ]

    with connection.cursor() as cursor:
        existing = already_enqueued(cursor, [event_id for _entry, _start, event_id in wanted])

        propagated = 0
        enqueued = 0
        propagators: dict[str, Propagator] = {}

        for entry, start, event_id in wanted:
            if event_id in existing:
                continue

            # Built once per satellite per tick and reused across its buckets.
            # Construction is the expensive half of a propagator — the SGP4
            # record and the Skyfield wrapper — and rebuilding it per bucket
            # would cost more than the sampling it enables.
            propagator = propagators.get(entry.satellite_id)
            if propagator is None:
                propagator = Propagator(entry.element_set)
                propagators[entry.satellite_id] = propagator

            track = sample_track(
                entry.satellite_id,
                propagator,
                start,
                start + timedelta(seconds=policy.bucket_s),
                policy.interval_s,
            )
            propagated += 1

            message = build_event(track, entry.element_set, now, staleness, headers)
            if enqueue_once(cursor, message):
                enqueued += 1

    return SweepStats(
        satellites=len(entries),
        buckets=len(starts),
        tracks_propagated=propagated,
        enqueued=enqueued,
        already_published=len(existing),
    )


def retry_delay(stats: SweepStats, tick_s: float) -> float:
    """How long to wait before sweeping again.

    An UNSEEDED database is not a normal tick, and waiting the full interval
    for it is a defect rather than a tuning choice. The worker starts before
    `make seed` runs on a cold stack, so the first sweep finds no element sets;
    at the default 300s tick the constellation then has no ephemeris — and
    `overpass_tle_age_hours` no series at all — for five minutes afterwards.
    That is the exact window in which someone is most likely to be watching a
    fresh stack, and the TLE staleness alert is blind for the whole of it.

    Found by the dashboard gate failing in CI on a genuinely cold stack. It
    passes against a long-running local instance that has swept many times
    already, which is precisely the difference a cold-start gate exists to
    expose.

    A separate function so the rule is testable without driving the loop: the
    interesting behaviour is one comparison, and asserting it through mocked
    asyncio timers would test the mocks.
    """
    return tick_s if stats.satellites else _UNSEEDED_RETRY_S


async def run(
    config: SweeperConfig,
    stop: asyncio.Event | None = None,
    max_iterations: int | None = None,
    clock: Any = None,
) -> SweepStats:
    """Sweep on a timer until stopped.

    `max_iterations` exists for the same reason the relay's does: so a test can
    run the real loop to completion rather than cancelling it and hoping the
    shutdown path works.

    The sweep runs in a thread. Propagating a constellation is seconds of solid
    numpy and psycopg is synchronous, so doing either on the event loop would
    stop the process answering anything else — the same reason the worker's
    handler runs off the loop.
    """
    stop = stop or asyncio.Event()
    clock = clock or (lambda: datetime.now(UTC))
    total = SweepStats()

    with psycopg.connect(config.dsn, autocommit=True) as connection:
        iterations = 0
        while not stop.is_set() and (max_iterations is None or iterations < max_iterations):
            iterations += 1
            # A span per tick, and the traceparent it produces travels on every
            # event the tick enqueues. There is no incoming trace to continue —
            # nothing published a timer — so this span is a root, and that is
            # the honest shape: the sweep genuinely starts here.
            try:
                with telemetry.tracer().start_as_current_span(
                    "ephemeris.sweep",
                    kind=trace.SpanKind.PRODUCER,
                ) as span:
                    stats = await asyncio.to_thread(
                        sweep_once,
                        connection,
                        clock(),
                        config.policy,
                        None,
                        telemetry.trace_headers(),
                    )
                    span.set_attribute("overpass.ephemeris.enqueued", stats.enqueued)
                    span.set_attribute("overpass.ephemeris.propagated", stats.tracks_propagated)
                    span.set_attribute("overpass.ephemeris.satellites", stats.satellites)
            except Exception:
                stats = SweepStats()
                # Keep ticking. A failed sweep is almost always the database
                # reconnecting or the constellation not yet seeded, and exiting
                # would turn a blip into a globe that never gets an orbit again.
                log.exception("ephemeris sweep failed; will retry on the next tick")
            else:
                if stats.enqueued:
                    log.info(
                        "ephemeris sweep enqueued %d tracks across %d satellites",
                        stats.enqueued,
                        stats.satellites,
                    )
                total = SweepStats(
                    satellites=stats.satellites,
                    buckets=stats.buckets,
                    tracks_propagated=total.tracks_propagated + stats.tracks_propagated,
                    enqueued=total.enqueued + stats.enqueued,
                    already_published=stats.already_published,
                )

            # An UNSEEDED database is not a normal tick, and waiting the full
            # interval for it is a real defect rather than a tuning choice.
            #
            # The worker starts before `make seed` runs on a cold stack, so the
            # first sweep finds nothing, and at the default 300s tick the
            # constellation then has no ephemeris — and overpass_tle_age_hours
            # no series at all — for five minutes after every cold start. The
            # TLE staleness alert is blind for exactly that window, which is
            # the window in which someone is most likely to be watching.
            #
            # Found by the dashboard gate failing in CI on a genuinely cold
            # stack. It passes locally because a long-running instance has
            # swept many times already, which is precisely the kind of
            # difference a cold-start gate exists to expose.
            delay = retry_delay(stats, config.tick_s)
            with contextlib.suppress(TimeoutError):
                await asyncio.wait_for(stop.wait(), timeout=delay)

    return total
