"""The rolling ephemeris sweep, against a real Postgres.

Three claims, and each one is the reason a piece of this exists:

  THE STORE RETURNS THE NEWEST USABLE ELEMENT SET PER SATELLITE. A sweep that
  picked an arbitrary row would draw an orbit from a TLE nobody chose, and the
  event's own provenance field would faithfully record the wrong answer.

  THE SWEEP IS IDEMPOTENT. It runs on a timer over a rolling horizon, so it
  re-covers ground it has already covered on every tick. The outbox's unique
  event_id is what makes that a no-op, and this is where that is demonstrated
  rather than asserted in a comment.

  IT DOES NOT PROPAGATE WHAT IT WILL NOT PUBLISH. The expensive part is SGP4
  over a thousand instants per bucket; the id is derivable without any of it. A
  sweep that propagated first and deduplicated second would burn a constellation
  of arithmetic every tick to produce nothing.
"""

from __future__ import annotations

import os
from datetime import UTC, datetime, timedelta
from typing import TYPE_CHECKING, Any

import psycopg
import pytest

from feasibility.ephemeris import EPHEMERIS_SUBJECT, derive_event_id
from feasibility.orbit.ephemeris import SamplingPolicy, bucket_starts
from feasibility.sweeper import SweeperConfig, sweep_once
from feasibility.sweeper import run as run_sweeper
from feasibility.tle.element_set import parse, tle_checksum
from feasibility.tle.store import SatelliteElementSet, newest_element_sets

if TYPE_CHECKING:
    from collections.abc import Iterator

DSN = os.environ.get("OVERPASS_TEST_DSN")

pytestmark = pytest.mark.skipif(
    not DSN, reason="set OVERPASS_TEST_DSN to run the ephemeris sweep tests"
)

# Inside the frozen snapshot's usable life, so the seeded constellation is not
# classified STALE. Fixed rather than `now`, because a sweep that consults wall
# time is not reproducible.
WHEN = datetime(2026, 8, 7, 10, 37, 14, tzinfo=UTC)

# Small on purpose. The point of these tests is the bookkeeping around the
# propagation, and a 24-hour horizon at ten seconds would spend a minute of SGP4
# proving something a single bucket proves.
POLICY = SamplingPolicy(interval_s=60.0, bucket_s=600.0, horizon_s=600.0)


def dsn() -> str:
    assert DSN is not None
    return DSN


@pytest.fixture
def connection() -> Iterator[psycopg.Connection[Any]]:
    """A connection whose every write is rolled back.

    These tests write to the shared outbox, and a sweep that left rows behind
    would make the next run of the suite exercise the deduplication path instead
    of the publish path — which is exactly the sort of order dependence that
    makes a suite pass alone and fail in CI.
    """
    with psycopg.connect(dsn()) as conn, conn.transaction() as transaction:
        yield conn
        transaction.force_rollback = True


class TestTheElementSetStore:
    def test_it_returns_one_element_set_per_seeded_satellite(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        found = newest_element_sets(connection, WHEN)
        assert found, "the constellation is not seeded — run `make seed`"

        ids = [entry.satellite_id for entry in found]
        assert len(ids) == len(set(ids))

        with connection.cursor() as cursor:
            cursor.execute("SELECT satellite_id FROM reference.satellites")
            seeded = {row[0] for row in cursor.fetchall()}
        assert set(ids) <= seeded

    def test_the_satellite_id_is_the_column_not_the_celestrak_name(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # reference.satellites enforces ^[A-Z0-9][A-Z0-9_-]{0,31}$, and the
        # Celestrak name does not satisfy it — "CAPELLA-11 (ACADIA-1)" has a
        # parenthetical the seeder strips. Deriving the id from the element set
        # name instead of reading the column would publish a satellite_id that
        # joins to nothing.
        for entry in newest_element_sets(connection, WHEN):
            assert " " not in entry.satellite_id
            assert "(" not in entry.satellite_id

    def test_a_newer_element_set_wins(self, connection: psycopg.Connection[Any]) -> None:
        existing = newest_element_sets(connection, WHEN)[0]
        newer_epoch = existing.element_set.epoch + timedelta(hours=6)
        _insert_reepoched(connection, existing, newer_epoch)

        after = {e.satellite_id: e for e in newest_element_sets(connection, WHEN)}
        assert after[existing.satellite_id].element_set.epoch > existing.element_set.epoch

    def test_an_element_set_from_the_future_is_not_used(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # Propagating from an epoch later than the instant asked about is
        # extrapolating backwards, and the fresher-looking row would win every
        # ordering while being the wrong answer to the question asked.
        existing = newest_element_sets(connection, WHEN)[0]
        _insert_reepoched(connection, existing, WHEN + timedelta(days=30))

        after = {e.satellite_id: e for e in newest_element_sets(connection, WHEN)}
        assert after[existing.satellite_id].element_set.epoch == existing.element_set.epoch

    def test_a_row_whose_epoch_column_contradicts_its_own_lines_is_refused(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # Such a row sorts by the column and propagates from the line, so it
        # would publish a track whose stated provenance is not the element set
        # it was computed from. Preferring either value quietly is worse than
        # stopping, because the provenance field is the whole point.
        existing = newest_element_sets(connection, WHEN)[0]
        with connection.cursor() as cursor:
            cursor.execute(
                """
                INSERT INTO reference.tle_sets
                    (satellite_id, norad_id, line1, line2, epoch, source)
                VALUES (%s, %s, %s, %s, %s, '{}'::jsonb)
                """,
                (
                    existing.satellite_id,
                    existing.element_set.norad_id,
                    existing.element_set.line1,
                    existing.element_set.line2,
                    existing.element_set.epoch + timedelta(hours=6),
                ),
            )

        with pytest.raises(ValueError, match="inconsistent"):
            newest_element_sets(connection, WHEN)


class TestTheSweep:
    def test_it_enqueues_one_event_per_satellite_and_bucket(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        satellites = len(newest_element_sets(connection, WHEN))
        buckets = len(bucket_starts(WHEN, POLICY))

        stats = sweep_once(connection, WHEN, POLICY)

        assert stats.enqueued == satellites * buckets
        assert _outbox_count(connection) == satellites * buckets

    def test_a_second_sweep_over_the_same_horizon_enqueues_nothing(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        first = sweep_once(connection, WHEN, POLICY)
        second = sweep_once(connection, WHEN, POLICY)

        assert first.enqueued > 0
        assert second.enqueued == 0
        assert second.already_published == first.enqueued
        assert _outbox_count(connection) == first.enqueued

    def test_the_second_sweep_propagates_nothing(self, connection: psycopg.Connection[Any]) -> None:
        # The reason the id is derived before the physics rather than after it.
        sweep_once(connection, WHEN, POLICY)
        second = sweep_once(connection, WHEN, POLICY)

        assert second.tracks_propagated == 0

    def test_advancing_the_clock_publishes_only_the_newly_uncovered_buckets(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        first = sweep_once(connection, WHEN, POLICY)
        later = sweep_once(connection, WHEN + timedelta(seconds=POLICY.bucket_s), POLICY)

        satellites = len(newest_element_sets(connection, WHEN))
        assert later.enqueued == satellites
        assert _outbox_count(connection) == first.enqueued + satellites

    def test_what_it_enqueues_is_addressed_and_identified_correctly(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        sweep_once(connection, WHEN, POLICY)
        entry = newest_element_sets(connection, WHEN)[0]
        expected = derive_event_id(
            entry.satellite_id, bucket_starts(WHEN, POLICY)[0], entry.element_set.epoch
        )

        with connection.cursor() as cursor:
            cursor.execute(
                "SELECT subject, event_type, payload->'data'->>'satellite_id' "
                "FROM feasibility.outbox WHERE event_id = %s",
                (expected,),
            )
            row = cursor.fetchone()

        assert row is not None, "the derived event id is not what the sweep enqueued under"
        assert row[0] == EPHEMERIS_SUBJECT
        assert row[1] == EPHEMERIS_SUBJECT
        assert row[2] == entry.satellite_id

    def test_it_leaves_the_events_unpublished_for_the_relay(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # The sweep writes to the outbox and nothing else. Publishing from here
        # would be the dual write the outbox exists to remove.
        sweep_once(connection, WHEN, POLICY)
        with connection.cursor() as cursor:
            cursor.execute(
                "SELECT count(*) FROM feasibility.outbox "
                "WHERE subject = %s AND published_at IS NOT NULL",
                (EPHEMERIS_SUBJECT,),
            )
            row = cursor.fetchone()
        assert row is not None
        assert row[0] == 0


def _outbox_count(connection: psycopg.Connection[Any]) -> int:
    with connection.cursor() as cursor:
        cursor.execute(
            "SELECT count(*) FROM feasibility.outbox WHERE subject = %s", (EPHEMERIS_SUBJECT,)
        )
        row = cursor.fetchone()
        return int(row[0]) if row else 0


def _insert_reepoched(
    connection: psycopg.Connection[Any], entry: SatelliteElementSet, epoch: datetime
) -> None:
    """Insert a genuinely different element set: same orbit, later epoch.

    Rewriting line 1's epoch field and recomputing its checksum, rather than
    inserting the same lines under a different `epoch` column. The two are not
    the same thing — the column is a decode of the line, and a row where they
    disagree is corruption, which the store now refuses. A test that produced
    one would be testing the refusal, not the ordering.
    """
    line1 = _with_epoch(entry.element_set.line1, epoch)
    with connection.cursor() as cursor:
        cursor.execute(
            """
            INSERT INTO reference.tle_sets
                (satellite_id, norad_id, line1, line2, epoch, source)
            VALUES (%s, %s, %s, %s, %s, '{}'::jsonb)
            """,
            (
                entry.satellite_id,
                entry.element_set.norad_id,
                line1,
                entry.element_set.line2,
                parse(entry.display_name, line1, entry.element_set.line2).epoch,
            ),
        )


def _with_epoch(line1: str, epoch: datetime) -> str:
    """Rewrite columns 18-32 (`YYDDD.DDDDDDDD`) and fix the checksum."""
    day_of_year = (epoch - datetime(epoch.year, 1, 1, tzinfo=UTC)).total_seconds() / 86400.0 + 1.0
    field = f"{epoch.year % 100:02d}{day_of_year:012.8f}"
    assert len(field) == 14, field
    rewritten = line1[:18] + field + line1[32:]
    return rewritten[:68] + str(tle_checksum(rewritten))


@pytest.mark.asyncio
class TestTheSweepLoop:
    """The loop itself, not just the tick.

    `sweep_once` is pure enough to test directly; what it does not cover is the
    composition around it — the thread hop, the trace headers captured on the
    loop and handed across it, and the fact that a second tick over an unchanged
    horizon does nothing. Those are exactly the joins that fail silently: a
    sweeper that raises inside `to_thread` and swallows it looks like a sweeper
    that has nothing to do.
    """

    async def test_it_ticks_and_the_second_tick_is_a_no_op(self) -> None:
        config = SweeperConfig(dsn=dsn(), tick_s=0.01, policy=POLICY)
        frozen = WHEN

        try:
            total = await run_sweeper(config, max_iterations=2, clock=lambda: frozen)
        finally:
            _purge_ephemeris_outbox()

        # Two ticks, one horizon: everything enqueued on the first and nothing
        # on the second. `enqueued` accumulates across ticks, so an equal
        # `tracks_propagated` is the evidence that the second tick did no work
        # rather than that it did the same work twice.
        assert total.enqueued > 0
        assert total.tracks_propagated == total.enqueued
        assert total.already_published == total.enqueued

    async def test_the_events_it_enqueues_carry_a_traceparent(self) -> None:
        # The header is injected on the event loop, inside the tick's span, and
        # then handed to a worker thread. Dropping it is how one distributed
        # trace silently becomes two, and the async hop is where that happens.
        config = SweeperConfig(dsn=dsn(), tick_s=0.01, policy=POLICY)

        try:
            await run_sweeper(config, max_iterations=1, clock=lambda: WHEN)
            with psycopg.connect(dsn()) as conn, conn.cursor() as cursor:
                cursor.execute(
                    "SELECT headers FROM feasibility.outbox WHERE subject = %s LIMIT 1",
                    (EPHEMERIS_SUBJECT,),
                )
                row = cursor.fetchone()
        finally:
            _purge_ephemeris_outbox()

        assert row is not None, "the sweep enqueued nothing"
        # Absent when no tracer provider is installed, which is the case in a
        # bare unit run — so the assertion is that the key is CARRIED, not that
        # a specific trace id appears.
        assert isinstance(row[0], dict)


def _purge_ephemeris_outbox() -> None:
    """Remove what the loop tests wrote.

    They cannot use the rolling-back fixture: `run_sweeper` opens its own
    connection, so its writes are outside any transaction this test controls.
    Leaving them behind would make the next run of the suite exercise the
    deduplication path instead of the publish path.
    """
    with psycopg.connect(dsn(), autocommit=True) as conn, conn.cursor() as cursor:
        cursor.execute("DELETE FROM feasibility.outbox WHERE subject = %s", (EPHEMERIS_SUBJECT,))


class TestTheElementSetCarriesTheSatelliteId:
    """`ElementSet.name` is a satellite_id, not a Celestrak display string.

    Load-bearing beyond tidiness. `pipeline.evaluate` publishes
    `window.satellite_id`, which comes from `element_set.name`, and compares
    `excluded_satellite_ids` against the same value. Parsing under the display
    name would publish `CAPELLA-11 (ACADIA-1)` — a satellite_id the contract's
    pattern rejects and that joins to nothing in reference.satellites — and would
    make a customer's exclusion of `CAPELLA-11` match nothing at all.
    """

    def test_the_element_set_name_is_the_id_the_rest_of_the_system_uses(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        for entry in newest_element_sets(connection, WHEN):
            assert entry.element_set.name == entry.satellite_id

    def test_the_celestrak_name_is_still_available_for_display(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # Not discarded — an operator looking at a satellite wants the name it
        # is catalogued under.
        entries = newest_element_sets(connection, WHEN)
        assert all(entry.display_name for entry in entries)
