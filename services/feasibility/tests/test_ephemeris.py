"""The ephemeris sweep: sampling, bucketing, and the event it publishes.

Where the physics is checked: `test_golden_orbital.py`. A sampled track is only
as good as the propagation under it, and that is cross-checked there against an
independent frame chain rather than against itself.

What is checked HERE is everything the sampling adds on top of the propagation,
which is exactly the part a golden orbital test cannot see:

  THE TUPLE ORDER. A sample is [t, lon, lat, alt] and a swap renders a satellite
  in the wrong hemisphere while every number stays in range.

  THE BUCKET GRID. Buckets are aligned rather than relative to whenever the
  sweep happened to run, because the event id is derived from the bucket start
  and an unaligned grid would make every sweep publish a new, overlapping track.

  THE DERIVED EVENT ID. This is the only event in the system whose id is not
  inherited from an upstream message, and it is what makes a rolling sweep
  idempotent at the outbox.
"""

from __future__ import annotations

import json
import math
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest

from feasibility.ephemeris import EPHEMERIS_SUBJECT, build_event, derive_event_id
from feasibility.orbit import Propagator
from feasibility.orbit.ephemeris import SamplingPolicy, bucket_starts, sample_track
from feasibility.tle.element_set import StalenessPolicy, parse_catalogue

ROOT = Path(__file__).resolve().parents[3]
SNAPSHOT = ROOT / "testdata" / "tle" / "sar-constellation.2026-08-07.tle"

# The instant the frozen snapshot is about. Fixed rather than `now`, because a
# sweep that consults wall time is not reproducible and these assertions depend
# on it being.
WHEN = datetime(2026, 8, 7, 10, 37, 14, tzinfo=UTC)


@pytest.fixture(scope="module")
def sentinel() -> Propagator:
    catalogue = {es.name: es for es in parse_catalogue(SNAPSHOT.read_text())}
    return Propagator(catalogue["SENTINEL-1A"])


class TestSampling:
    def test_a_bucket_is_sampled_half_open_at_the_requested_interval(
        self, sentinel: Propagator
    ) -> None:
        start = datetime(2026, 8, 7, 9, tzinfo=UTC)
        track = sample_track("SENTINEL-1A", sentinel, start, start + timedelta(minutes=5), 10.0)

        assert len(track.samples) == 30
        assert [s[0] for s in track.samples] == [float(10 * i) for i in range(30)]
        # Half-open, like every other interval in this system. The sample at
        # 300 s belongs to the next bucket; emitting it in both would put two
        # positions at one instant and make the projection's primary key the
        # arbiter of which one wins.
        assert track.samples[-1][0] == 290.0
        assert track.epoch == start
        assert track.horizon_end == start + timedelta(minutes=5)

    def test_a_sample_is_offset_longitude_latitude_altitude_in_that_order(
        self, sentinel: Propagator
    ) -> None:
        # The one failure that renders perfectly happily and is completely
        # wrong. Compared against the propagator's own typed subpoint, so this
        # tests the packing here and not the physics underneath it.
        start = datetime(2026, 8, 7, 9, tzinfo=UTC)
        track = sample_track("SENTINEL-1A", sentinel, start, start + timedelta(seconds=30), 10.0)

        for offset, longitude, latitude, altitude in track.samples:
            subpoint = sentinel.subpoint(start + timedelta(seconds=offset))
            assert longitude == pytest.approx(subpoint.longitude_deg, abs=1e-9)
            assert latitude == pytest.approx(subpoint.latitude_deg, abs=1e-9)
            assert altitude == pytest.approx(subpoint.elevation_m, abs=1e-6)

    def test_a_track_that_cannot_hold_two_samples_is_refused(self, sentinel: Propagator) -> None:
        # One point is a position, not a path, and the contract's minItems says
        # so. Refusing here means the publisher never has to ask.
        start = datetime(2026, 8, 7, 9, tzinfo=UTC)
        with pytest.raises(ValueError, match="at least two samples"):
            sample_track("SENTINEL-1A", sentinel, start, start + timedelta(seconds=10), 10.0)

    def test_consecutive_samples_are_one_interval_of_ground_track_apart(
        self, sentinel: Propagator
    ) -> None:
        # A cheap physical oracle that needs no library: a LEO ground track
        # moves at roughly 6.6 km/s, so ten seconds is roughly 66 km. This
        # catches a sample loop that reuses one instant, or steps by the wrong
        # unit — both of which produce a well-formed track of the right length.
        start = datetime(2026, 8, 7, 9, tzinfo=UTC)
        track = sample_track("SENTINEL-1A", sentinel, start, start + timedelta(minutes=5), 10.0)

        for previous, current in zip(track.samples, track.samples[1:], strict=False):
            mean_lat = math.radians((previous[2] + current[2]) / 2.0)
            delta_lon = (current[1] - previous[1] + 180.0) % 360.0 - 180.0
            separation_km = math.hypot(
                (current[2] - previous[2]) * 111.32,
                delta_lon * 111.32 * math.cos(mean_lat),
            )
            assert 50.0 < separation_km < 80.0


class TestBucketGrid:
    def test_buckets_are_aligned_to_the_grid_not_to_the_sweep(self) -> None:
        # The whole point. A sweep at 10:37 must produce the 09:00 bucket, not a
        # 10:37 bucket — otherwise the next sweep three minutes later derives a
        # different event id for an overlapping track and the outbox's unique
        # constraint stops protecting anything.
        policy = SamplingPolicy(bucket_s=3 * 3600, horizon_s=24 * 3600)
        starts = bucket_starts(WHEN, policy)

        assert starts[0] == datetime(2026, 8, 7, 9, tzinfo=UTC)
        assert all(s.hour % 3 == 0 and s.minute == 0 and s.second == 0 for s in starts)

    def test_the_grid_covers_the_whole_horizon(self) -> None:
        policy = SamplingPolicy(bucket_s=3 * 3600, horizon_s=24 * 3600)
        starts = bucket_starts(WHEN, policy)

        assert starts[0] <= WHEN
        assert starts[-1] + timedelta(seconds=policy.bucket_s) >= WHEN + timedelta(
            seconds=policy.horizon_s
        )
        assert starts == sorted(starts)
        assert len(set(starts)) == len(starts)


class TestDerivedEventId:
    def test_the_same_bucket_and_element_set_derive_the_same_id(self) -> None:
        start = datetime(2026, 8, 7, 9, tzinfo=UTC)
        epoch = datetime(2026, 8, 6, 21, 41, 12, tzinfo=UTC)

        assert derive_event_id("SENTINEL-1A", start, epoch) == derive_event_id(
            "SENTINEL-1A", start, epoch
        )

    def test_a_fresher_element_set_derives_a_different_id(self) -> None:
        # So the newer track is published rather than swallowed as a duplicate.
        start = datetime(2026, 8, 7, 9, tzinfo=UTC)
        first = derive_event_id("SENTINEL-1A", start, datetime(2026, 8, 6, tzinfo=UTC))
        second = derive_event_id("SENTINEL-1A", start, datetime(2026, 8, 7, tzinfo=UTC))

        assert first != second

    def test_each_satellite_and_bucket_gets_its_own_id(self) -> None:
        epoch = datetime(2026, 8, 6, tzinfo=UTC)
        start = datetime(2026, 8, 7, 9, tzinfo=UTC)

        assert derive_event_id("SENTINEL-1A", start, epoch) != derive_event_id(
            "SENTINEL-1B", start, epoch
        )
        assert derive_event_id("SENTINEL-1A", start, epoch) != derive_event_id(
            "SENTINEL-1A", start + timedelta(hours=3), epoch
        )

    def test_the_id_is_a_uuid(self) -> None:
        derived = derive_event_id("SENTINEL-1A", datetime(2026, 8, 7, 9, tzinfo=UTC), WHEN)
        assert uuid.UUID(derived).version == 5


class TestTheEventItPublishes:
    def test_the_envelope_and_data_match_the_contract(self, sentinel: Propagator) -> None:
        start = datetime(2026, 8, 7, 9, tzinfo=UTC)
        track = sample_track("SENTINEL-1A", sentinel, start, start + timedelta(minutes=1), 10.0)
        message = build_event(track, sentinel.element_set, WHEN, StalenessPolicy())

        assert message.subject == EPHEMERIS_SUBJECT
        assert message.event_type == EPHEMERIS_SUBJECT
        assert message.event_id == derive_event_id("SENTINEL-1A", start, sentinel.element_set.epoch)

        payload = message.payload
        assert payload["event_type"] == EPHEMERIS_SUBJECT
        assert payload["producer"] == "feasibility-service"
        # Caused by the passage of time. There is no antecedent event, and the
        # envelope permits null for exactly this case.
        assert payload["causation_id"] is None

        data = payload["data"]
        assert data["satellite_id"] == "SENTINEL-1A"
        assert data["sample_interval_s"] == 10.0
        assert data["sample_count"] == len(data["samples"]) == 6
        assert data["horizon"] == {
            "start": "2026-08-07T09:00:00Z",
            "end": "2026-08-07T09:01:00Z",
        }
        assert data["tle_reference"]["satellite_id"] == "SENTINEL-1A"
        assert data["tle_reference"]["norad_id"] == 39634
        assert data["tle_reference"]["staleness"] == "AGING"

    def test_the_reference_names_the_element_set_the_track_came_from(
        self, sentinel: Propagator
    ) -> None:
        # Provenance is the difference between "the orbit looks wrong" being a
        # question and being an answer.
        start = datetime(2026, 8, 7, 9, tzinfo=UTC)
        track = sample_track("SENTINEL-1A", sentinel, start, start + timedelta(minutes=1), 10.0)
        message = build_event(track, sentinel.element_set, WHEN, StalenessPolicy())

        reference = message.payload["data"]["tle_reference"]
        # Parsed back rather than string-compared against a copy of the
        # formatter: a test that reimplements the code it checks agrees with it
        # by construction. What matters is that the instant survives, to the
        # millisecond the contract's date-time format carries.
        published = datetime.fromisoformat(reference["tle_epoch"].replace("Z", "+00:00"))
        assert abs((published - sentinel.element_set.epoch).total_seconds()) < 0.001
        assert reference["tle_age_hours"] == pytest.approx(
            sentinel.element_set.age_hours(WHEN), abs=1e-6
        )

    def test_a_track_published_from_a_stale_element_set_says_so_rather_than_being_dropped(
        self, sentinel: Propagator
    ) -> None:
        # Deliberately unlike evaluate(), which REFUSES on a stale element set.
        # An access window is a commitment the planner acts on; a track is a
        # drawing that states its own age. Dropping it would empty the globe on
        # the fourth day of a demo for a reason the viewer cannot see.
        much_later = sentinel.element_set.epoch + timedelta(days=10)
        start = datetime(2026, 8, 7, 9, tzinfo=UTC)
        track = sample_track("SENTINEL-1A", sentinel, start, start + timedelta(minutes=1), 10.0)

        message = build_event(track, sentinel.element_set, much_later, StalenessPolicy())
        assert message.payload["data"]["tle_reference"]["staleness"] == "STALE"

    def test_it_records_what_it_publishes_for_the_contract_gate(self, sentinel: Propagator) -> None:
        """Write the real event where scripts/contracts_validate.py will find it.

        Not an assertion — the assertion happens in `make contracts-validate`,
        against the schema itself. #124 is the reason: a producer whose tests
        build the payload with the same helper they assert against will agree
        with itself forever while emitting something the contract rejects. The
        only cure is showing the schema what the producer really emits.
        """
        start = datetime(2026, 8, 7, 9, tzinfo=UTC)
        track = sample_track("SENTINEL-1A", sentinel, start, start + timedelta(minutes=2), 10.0)
        message = build_event(track, sentinel.element_set, WHEN, StalenessPolicy())

        destination = (
            Path(__file__).resolve().parents[1]
            / "testdata"
            / "published-events"
            / EPHEMERIS_SUBJECT
            / "one-bucket.json"
        )
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(json.dumps(message.payload, indent=2) + "\n")

        assert json.loads(destination.read_text())["data"]["sample_count"] == 12
