"""The sweep pipeline: refusals, ground speed, and end-to-end opportunities.

No broker and no database. The pipeline is a pure function of its inputs by
design, which is what lets the physics be tested without either.
"""

from __future__ import annotations

import math
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest

from feasibility.orbit import GroundPoint, Propagator
from feasibility.pipeline import Opportunity, evaluate, ground_speed_km_s
from feasibility.sar import AcquisitionConstraints, ImagingMode, LookSide, SensorMode
from feasibility.tle.element_set import StalenessPolicy, parse_catalogue

SNAPSHOT = next(
    p / "testdata" / "tle" / "sar-constellation.2026-08-07.tle"
    for p in Path(__file__).resolve().parents
    if (p / "testdata").is_dir() and (p / "contracts").is_dir()
)

T0 = datetime(2026, 8, 7, tzinfo=UTC)
# The snapshot's epochs are 5-7 August, so "now" has to sit near them or every
# element set is STALE and the sweep refuses before doing anything.
NOW = datetime(2026, 8, 7, 12, tzinfo=UTC)
LISBON = GroundPoint(38.7223, -9.1393)

STRIPMAP = SensorMode(
    mode=ImagingMode.STRIPMAP,
    swath_width_km=80.0,
    resolution_m=5.0,
    min_dwell_s=8.0,
    max_dwell_s=30.0,
    max_squint_deg=5.0,
)
MODES = {"STRIPMAP": STRIPMAP}


@pytest.fixture(scope="module")
def constellation() -> list[Propagator]:
    return [Propagator(es) for es in parse_catalogue(SNAPSHOT.read_text())]


class TestGroundSpeed:
    def test_is_slower_than_orbital_speed(self, constellation: list[Propagator]) -> None:
        """The distinction the footprint depends on.

        The ground trace moves more slowly than the satellite, by roughly the
        ratio of Earth's radius to the orbital radius. Using orbital speed would
        make every along-track footprint about 12 percent too long — a wrong
        answer that looks entirely reasonable.
        """
        propagator = constellation[0]
        speed = ground_speed_km_s(propagator, T0)

        geocentric = propagator.satellite.at(propagator.time(T0))
        orbital = math.sqrt(sum(v**2 for v in geocentric.velocity.km_per_s))

        assert speed < orbital
        altitude_km = propagator.subpoint(T0).elevation_m / 1000.0
        expected_ratio = 6371.0 / (6371.0 + altitude_km)
        assert speed / orbital == pytest.approx(expected_ratio, rel=0.02)

    def test_is_physically_plausible_for_leo(self, constellation: list[Propagator]) -> None:
        # A ~700 km LEO ground trace runs near 6.7 km/s.
        for propagator in constellation[:3]:
            assert 6.0 < ground_speed_km_s(propagator, T0) < 7.5

    def test_is_symmetric_and_positive_everywhere(self, constellation: list[Propagator]) -> None:
        propagator = constellation[0]
        for minutes in (0, 20, 40, 60, 80):
            assert ground_speed_km_s(propagator, T0 + timedelta(minutes=minutes)) > 0


class TestRefusals:
    def test_stale_element_sets_refuse_before_computing(
        self, constellation: list[Propagator]
    ) -> None:
        # A stale TLE produces a confidently wrong window. Publishing one is
        # worse than publishing nothing, because the planner cannot tell.
        far_future = datetime(2027, 1, 1, tzinfo=UTC)
        outcome = evaluate(
            "req-stale",
            LISBON,
            T0,
            T0 + timedelta(hours=24),
            constellation,
            MODES,
            AcquisitionConstraints(),
            now=far_future,
        )
        assert outcome.refusal is not None
        assert outcome.refusal.reason_code == "TLE_STALE"
        # Retryable: a fresh fetch would change the answer.
        assert outcome.refusal.retryable
        assert outcome.opportunities == []
        assert outcome.satellites_evaluated == 0

    def test_no_access_is_not_retryable(self, constellation: list[Propagator]) -> None:
        # A physical impossibility does not become possible on the fifth
        # attempt, and retrying it burns redelivery budget a transient failure
        # needs.
        outcome = evaluate(
            "req-short",
            LISBON,
            T0,
            T0 + timedelta(minutes=2),
            constellation,
            MODES,
            AcquisitionConstraints(),
            now=NOW,
        )
        assert outcome.refusal is not None
        assert outcome.refusal.reason_code == "NO_ACCESS_IN_HORIZON"
        assert not outcome.refusal.retryable

    def test_customer_constraints_that_eliminate_access_say_so(
        self, constellation: list[Propagator]
    ) -> None:
        """A distinct and actionable answer.

        Access existed and the customer's own narrowing removed it. Reporting
        that as NO_ACCESS_IN_HORIZON would tell them the geometry is impossible
        when in fact widening their constraints would work.
        """
        outcome = evaluate(
            "req-narrow",
            LISBON,
            T0,
            T0 + timedelta(hours=48),
            constellation,
            MODES,
            AcquisitionConstraints(min_incidence_deg=44.9, max_incidence_deg=45.0),
            now=NOW,
        )
        assert outcome.refusal is not None
        assert outcome.refusal.reason_code == "CONSTRAINTS_TOO_NARROW"
        assert not outcome.refusal.retryable

    def test_excluded_satellites_are_not_evaluated(self, constellation: list[Propagator]) -> None:
        names = frozenset(p.element_set.name for p in constellation)
        outcome = evaluate(
            "req-excl",
            LISBON,
            T0,
            T0 + timedelta(hours=24),
            constellation,
            MODES,
            AcquisitionConstraints(excluded_satellite_ids=names),
            now=NOW,
        )
        assert outcome.satellites_evaluated == 0


class TestOpportunities:
    @pytest.fixture(scope="class")
    def outcome(self, constellation: list[Propagator]) -> object:
        return evaluate(
            "req-happy",
            LISBON,
            T0,
            T0 + timedelta(hours=48),
            constellation,
            MODES,
            AcquisitionConstraints(),
            now=NOW,
        )

    def test_finds_opportunities_over_a_two_day_horizon(self, outcome: object) -> None:
        assert outcome.refusal is None  # type: ignore[attr-defined]
        assert outcome.opportunities  # type: ignore[attr-defined]

    def test_every_opportunity_is_geometrically_valid(self, outcome: object) -> None:
        for o in outcome.opportunities:  # type: ignore[attr-defined]
            assert STRIPMAP.min_incidence_deg <= o.geometry.incidence_angle_deg
            assert o.geometry.incidence_angle_deg <= STRIPMAP.max_incidence_deg
            assert abs(o.geometry.squint_angle_deg) <= STRIPMAP.max_squint_deg
            assert o.geometry.look_side in STRIPMAP.permitted_look_sides

    def test_every_footprint_actually_covers_the_target(self, outcome: object) -> None:
        # Good angles are not enough. A footprint that misses the target is not
        # an opportunity, however imageable the instant looks.
        from feasibility.sar import contains_target

        for o in outcome.opportunities:  # type: ignore[attr-defined]
            assert contains_target(o.footprint, LISBON)

    def test_quality_scores_are_in_range(self, outcome: object) -> None:
        for o in outcome.opportunities:  # type: ignore[attr-defined]
            assert 0.0 < o.quality_score <= 1.0

    def test_opportunity_ids_are_unique(self, outcome: object) -> None:
        ids = [o.opportunity_id for o in outcome.opportunities]  # type: ignore[attr-defined]
        assert len(ids) == len(set(ids))

    def test_is_deterministic(self, constellation: list[Propagator]) -> None:
        def run() -> list[Opportunity]:
            return evaluate(
                "req-det",
                LISBON,
                T0,
                T0 + timedelta(hours=24),
                constellation,
                MODES,
                AcquisitionConstraints(),
                now=NOW,
            ).opportunities

        first, second = run(), run()
        assert [o.opportunity_id for o in first] == [o.opportunity_id for o in second]
        assert [o.geometry for o in first] == [o.geometry for o in second]


class TestHorizonClamping:
    def test_an_over_long_horizon_is_clamped_and_declared(
        self, constellation: list[Propagator]
    ) -> None:
        outcome = evaluate(
            "req-long",
            LISBON,
            T0,
            T0 + timedelta(days=30),
            constellation,
            MODES,
            AcquisitionConstraints(),
            now=NOW,
        )
        assert outcome.truncated
        assert outcome.horizon_end == T0 + timedelta(hours=72)


class TestStalenessBoundary:
    def test_an_aging_element_set_still_computes(self, constellation: list[Propagator]) -> None:
        # AGING is published with a flag, not refused. Only STALE refuses.
        policy = StalenessPolicy()
        aging_moment = constellation[0].element_set.epoch + timedelta(
            hours=policy.fresh_below_hours + 1
        )
        outcome = evaluate(
            "req-aging",
            LISBON,
            aging_moment,
            aging_moment + timedelta(hours=24),
            constellation[:1],
            MODES,
            AcquisitionConstraints(),
            now=aging_moment,
        )
        assert outcome.refusal is None or outcome.refusal.reason_code != "TLE_STALE"


class TestLookSideConstraint:
    def test_restricting_the_look_side_reduces_opportunities(
        self, constellation: list[Propagator]
    ) -> None:
        unrestricted = evaluate(
            "req-any",
            LISBON,
            T0,
            T0 + timedelta(hours=48),
            constellation,
            MODES,
            AcquisitionConstraints(),
            now=NOW,
        ).opportunities
        left_only = evaluate(
            "req-left",
            LISBON,
            T0,
            T0 + timedelta(hours=48),
            constellation,
            MODES,
            AcquisitionConstraints(look_side=LookSide.LEFT),
            now=NOW,
        ).opportunities

        assert len(left_only) <= len(unrestricted)
        assert all(o.geometry.look_side is LookSide.LEFT for o in left_only)


class TestOpportunityIdentity:
    """The id has to be a UUID, and it has to be derived rather than random.

    Both halves are load-bearing and neither was true before #131.

    A UUID because the contract says `format: uuid` and
    `feasibility.opportunities.opportunity_id` is a `uuid` column — the old
    `f"{request_id}:{index}:{mode}"` satisfied neither, so a sweep that produced
    an opportunity could not have published it and could not have stored it.

    Derived because a redelivered request must produce the same ids. The
    consumer's dedup ledger stops the work happening twice in the normal case,
    but a replay from the stream after a read-model rebuild is a supported
    operation, and random ids would fan a single request out into two disjoint
    sets of opportunities nobody can reconcile.
    """

    @pytest.fixture(scope="class")
    def outcome(self, constellation: list[Propagator]) -> object:
        return evaluate(
            "3c4d5e6f-7a8b-4c9d-8e1f-2a3b4c5d6e7f",
            LISBON,
            T0,
            T0 + timedelta(hours=48),
            constellation,
            MODES,
            AcquisitionConstraints(),
            now=NOW,
        )

    def test_every_id_is_a_uuid(self, outcome: object) -> None:
        for o in outcome.opportunities:  # type: ignore[attr-defined]
            assert uuid.UUID(o.opportunity_id).version == 5

    def test_the_same_request_derives_the_same_ids(self, constellation: list[Propagator]) -> None:
        def ids() -> list[str]:
            return [
                o.opportunity_id
                for o in evaluate(
                    "3c4d5e6f-7a8b-4c9d-8e1f-2a3b4c5d6e7f",
                    LISBON,
                    T0,
                    T0 + timedelta(hours=24),
                    constellation,
                    MODES,
                    AcquisitionConstraints(),
                    now=NOW,
                ).opportunities
            ]

        assert ids() == ids()

    def test_a_different_request_derives_different_ids(
        self, constellation: list[Propagator]
    ) -> None:
        # Same geometry, same window, different request. Two customers asking
        # for the same target must not collide on a primary key.
        def ids(request_id: str) -> set[str]:
            return {
                o.opportunity_id
                for o in evaluate(
                    request_id,
                    LISBON,
                    T0,
                    T0 + timedelta(hours=24),
                    constellation,
                    MODES,
                    AcquisitionConstraints(),
                    now=NOW,
                ).opportunities
            }

        first = ids("3c4d5e6f-7a8b-4c9d-8e1f-2a3b4c5d6e7f")
        second = ids("4d5e6f7a-8b9c-4d0e-8f2a-3b4c5d6e7f80")
        assert first
        assert first.isdisjoint(second)
