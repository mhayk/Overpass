"""Access-window search: correctness of the algorithm, not of the physics.

The physics is verified in test_vallado_vectors.py against the reference C++
output. What is left to get wrong here is the search: missing a pass between two
coarse samples, converging on the wrong side of a boundary, mishandling a window
the horizon cuts in half. Those are the failures that produce no error and a
request that merely looks infeasible.

The central test is `test_coarse_step_finds_the_same_windows_as_a_fine_step`.
Everything else is a boundary condition.
"""

from __future__ import annotations

import time
from datetime import UTC, datetime, timedelta
from itertools import pairwise
from pathlib import Path

import pytest

from feasibility.orbit import (
    AccessSearchPolicy,
    GroundPoint,
    HorizonPolicy,
    Propagator,
    search,
    sweep,
)
from feasibility.tle.element_set import parse, parse_catalogue

S1A_L1 = "1 39634U 14016A   26217.94446112  .00000085  00000+0  27560-4 0  9992"
S1A_L2 = "2 39634  98.1585 224.7210 0001387  84.4412 275.6946 14.59278904657278"

SNAPSHOT = next(
    p / "testdata" / "tle" / "sar-constellation.2026-08-07.tle"
    for p in Path(__file__).resolve().parents
    if (p / "testdata").is_dir() and (p / "contracts").is_dir()
)

LISBON = GroundPoint(latitude_deg=38.7223, longitude_deg=-9.1393)
HORIZON_START = datetime(2026, 8, 7, 0, 0, tzinfo=UTC)
HORIZON_END = HORIZON_START + timedelta(hours=24)


@pytest.fixture
def propagator() -> Propagator:
    return Propagator(parse("SENTINEL-1A", S1A_L1, S1A_L2))


class TestSearch:
    def test_finds_a_plausible_number_of_leo_passes(self, propagator: Propagator) -> None:
        # A sun-synchronous LEO at 98 degrees inclination gives a mid-latitude
        # site a handful of passes a day, in a morning and an evening group. A
        # result of zero, or of fifty, means something is structurally wrong
        # rather than slightly off.
        windows = search(propagator, LISBON, HORIZON_START, HORIZON_END)
        assert 2 <= len(windows) <= 8

    def test_pass_durations_are_physically_plausible(self, propagator: Propagator) -> None:
        # At a 5 degree mask a ~700 km LEO pass runs several minutes. Seconds
        # would mean the boundaries are wrong; an hour would mean the geometry
        # is not Earth-relative at all.
        for w in search(propagator, LISBON, HORIZON_START, HORIZON_END):
            assert 120.0 < w.duration_s < 1200.0

    def test_windows_are_ordered_and_disjoint(self, propagator: Propagator) -> None:
        windows = search(propagator, LISBON, HORIZON_START, HORIZON_END)
        for earlier, later in pairwise(windows):
            assert earlier.end < later.start

    def test_windows_lie_inside_the_horizon(self, propagator: Propagator) -> None:
        for w in search(propagator, LISBON, HORIZON_START, HORIZON_END):
            assert HORIZON_START <= w.start < w.end <= HORIZON_END

    def test_coarse_step_finds_the_same_windows_as_a_fine_step(
        self, propagator: Propagator
    ) -> None:
        """The load-bearing test for the coarse-to-fine design.

        A 60-second coarse step is only safe if no pass can hide between two
        samples. Rather than argue that from pass durations, compare against a
        5-second step, which is twelve times finer and cannot plausibly miss
        anything the coarse pass found — and, crucially, must not find anything
        the coarse pass missed.
        """
        coarse = search(propagator, LISBON, HORIZON_START, HORIZON_END)
        fine = search(
            propagator,
            LISBON,
            HORIZON_START,
            HORIZON_END,
            AccessSearchPolicy(coarse_step_s=5.0),
        )
        assert len(coarse) == len(fine), (
            f"coarse step found {len(coarse)} windows, fine step found {len(fine)} — "
            "the coarse step is missing a pass"
        )
        # Boundaries should also agree to within the refinement tolerance, since
        # both bisect to one second regardless of how they bracketed.
        for c, f in zip(coarse, fine, strict=True):
            assert abs((c.start - f.start).total_seconds()) <= 2.0
            assert abs((c.end - f.end).total_seconds()) <= 2.0

    def test_boundaries_sit_on_the_mask(self, propagator: Propagator) -> None:
        # Bisection converges to within a second, and elevation near the horizon
        # changes by well under a degree a second, so the elevation at each
        # boundary should be within a hair of the mask.
        policy = AccessSearchPolicy()
        for w in search(propagator, LISBON, HORIZON_START, HORIZON_END, policy):
            for edge in (w.start, w.end):
                elevation = propagator.topocentric(LISBON, edge).elevation_deg
                assert elevation == pytest.approx(policy.elevation_mask_deg, abs=0.2)

    def test_reported_boundaries_are_inside_the_window(self, propagator: Propagator) -> None:
        # A window must never claim visibility the geometry does not support:
        # both endpoints are at or above the mask, never below it.
        policy = AccessSearchPolicy()
        for w in search(propagator, LISBON, HORIZON_START, HORIZON_END, policy):
            for edge in (w.start, w.end):
                elevation = propagator.topocentric(LISBON, edge).elevation_deg
                assert elevation >= policy.elevation_mask_deg - 1e-6

    def test_peak_is_inside_the_window_and_is_the_maximum(self, propagator: Propagator) -> None:
        for w in search(propagator, LISBON, HORIZON_START, HORIZON_END):
            assert w.start <= w.peak_at <= w.end
            assert w.peak_elevation_deg >= propagator.topocentric(LISBON, w.start).elevation_deg
            assert w.peak_elevation_deg >= propagator.topocentric(LISBON, w.end).elevation_deg

    def test_determinism(self, propagator: Propagator) -> None:
        # Golden-reference tests in M1-12 are impossible without this.
        first = search(propagator, LISBON, HORIZON_START, HORIZON_END)
        second = search(propagator, LISBON, HORIZON_START, HORIZON_END)
        assert first == second

    def test_a_horizon_shorter_than_one_step_still_works(self, propagator: Propagator) -> None:
        # The grid must land exactly on the end, or a horizon shorter than a
        # single coarse step degenerates to one sample and finds nothing.
        short_end = HORIZON_START + timedelta(seconds=30)
        assert search(propagator, LISBON, HORIZON_START, short_end) == []

    def test_rejects_an_inverted_horizon(self, propagator: Propagator) -> None:
        with pytest.raises(ValueError, match="must be after"):
            search(propagator, LISBON, HORIZON_END, HORIZON_START)

    def test_rejects_a_naive_horizon(self, propagator: Propagator) -> None:
        with pytest.raises(ValueError, match="timezone-aware"):
            search(propagator, LISBON, datetime(2026, 8, 7), HORIZON_END)

    def test_a_higher_mask_never_yields_a_longer_window(self, propagator: Propagator) -> None:
        # Monotonicity: raising the mask can only shrink or drop windows. A
        # violation would mean the boundary search is picking the wrong side.
        low = search(propagator, LISBON, HORIZON_START, HORIZON_END)
        high = search(
            propagator,
            LISBON,
            HORIZON_START,
            HORIZON_END,
            AccessSearchPolicy(elevation_mask_deg=20.0),
        )
        assert len(high) <= len(low)
        assert sum(w.duration_s for w in high) < sum(w.duration_s for w in low)


class TestClipping:
    def test_a_window_open_at_the_horizon_start_is_marked_clipped(
        self, propagator: Propagator
    ) -> None:
        # Start the horizon in the middle of a known pass.
        pass_one = search(propagator, LISBON, HORIZON_START, HORIZON_END)[0]
        midpass = pass_one.start + (pass_one.end - pass_one.start) / 2
        (clipped,) = search(propagator, LISBON, midpass, pass_one.end - timedelta(seconds=1))
        assert clipped.clipped_at_start
        assert clipped.clipped_at_end
        assert clipped.start == midpass

    def test_an_ordinary_window_is_not_marked_clipped(self, propagator: Propagator) -> None:
        windows = search(propagator, LISBON, HORIZON_START, HORIZON_END)
        interior = [w for w in windows if not w.clipped_at_start and not w.clipped_at_end]
        assert interior, "expected at least one window fully inside the horizon"


class TestOrbitNumber:
    def test_increments_once_per_revolution(self, propagator: Propagator) -> None:
        # Mean motion is 14.59 revolutions a day, so consecutive passes about 98
        # minutes apart must be one orbit apart.
        period_minutes = 1440.0 / propagator.revolutions_per_day()
        base = HORIZON_START
        first = propagator.orbit_number(base)
        later = propagator.orbit_number(base + timedelta(minutes=period_minutes))
        assert later == first + 1

    def test_matches_the_element_set_revolution_count_at_epoch(
        self, propagator: Propagator
    ) -> None:
        at_epoch = propagator.orbit_number(propagator.element_set.epoch)
        assert at_epoch == int(propagator.satellite.model.revnum)

    def test_is_monotonic_across_the_horizon(self, propagator: Propagator) -> None:
        windows = search(propagator, LISBON, HORIZON_START, HORIZON_END)
        numbers = [w.orbit_number for w in windows]
        assert numbers == sorted(numbers)

    def test_rejects_a_naive_datetime(self, propagator: Propagator) -> None:
        with pytest.raises(ValueError, match="naive"):
            propagator.orbit_number(datetime(2026, 8, 7))


class TestGeometry:
    def test_incidence_and_elevation_are_complementary(self, propagator: Propagator) -> None:
        # The bridge between this module, which thinks in elevation, and the SAR
        # filter in M1-11, whose band is stated in incidence. If this identity
        # ever breaks, the two halves are measuring from different references
        # and every geometry decision downstream is wrong by the difference.
        for w in search(propagator, LISBON, HORIZON_START, HORIZON_END):
            topo = propagator.topocentric(LISBON, w.peak_at)
            assert topo.elevation_deg + topo.incidence_deg == pytest.approx(90.0)

    def test_slant_range_exceeds_orbital_altitude(self, propagator: Propagator) -> None:
        # A slant range shorter than the satellite's height above the ground is
        # geometrically impossible and would mean the topocentric vector is
        # being computed from the wrong origin.
        for w in search(propagator, LISBON, HORIZON_START, HORIZON_END):
            topo = propagator.topocentric(LISBON, w.peak_at)
            altitude_km = propagator.subpoint(w.peak_at).elevation_m / 1000.0
            assert topo.slant_range_km >= altitude_km

    def test_subpoint_is_on_the_globe(self, propagator: Propagator) -> None:
        sub = propagator.subpoint(HORIZON_START)
        assert -90.0 <= sub.latitude_deg <= 90.0
        assert -180.0 <= sub.longitude_deg <= 180.0
        # Sentinel-1A flies near 700 km.
        assert 600_000 < sub.elevation_m < 800_000

    @pytest.mark.parametrize(
        ("lat", "lon"),
        [(91.0, 0.0), (-91.0, 0.0), (0.0, 181.0), (0.0, -181.0)],
    )
    def test_rejects_impossible_ground_points(self, lat: float, lon: float) -> None:
        with pytest.raises(ValueError, match="out of range"):
            GroundPoint(latitude_deg=lat, longitude_deg=lon)


class TestPolicy:
    @pytest.mark.parametrize(
        "kwargs",
        [
            {"elevation_mask_deg": 90.0},
            {"elevation_mask_deg": -90.0},
            {"coarse_step_s": 0.0},
            {"refine_tolerance_s": 0.0},
            {"coarse_step_s": 1.0, "refine_tolerance_s": 1.0},
            {"coarse_step_s": 10.0, "refine_tolerance_s": 30.0},
        ],
    )
    def test_rejects_incoherent_policies(self, kwargs: dict[str, float]) -> None:
        with pytest.raises(ValueError):
            AccessSearchPolicy(**kwargs)

    def test_refuses_a_high_mask_with_the_default_step(self) -> None:
        # A high mask shortens passes below what a 60-second step can be trusted
        # to bracket. Refusing beats silently missing windows.
        with pytest.raises(ValueError, match="coarse step"):
            AccessSearchPolicy(elevation_mask_deg=45.0)

    def test_a_high_mask_is_allowed_with_a_deliberate_step(self) -> None:
        assert AccessSearchPolicy(elevation_mask_deg=45.0, coarse_step_s=10.0)


class TestSweep:
    def test_clamps_an_over_long_horizon_and_says_so(self, propagator: Propagator) -> None:
        result = sweep([propagator], LISBON, HORIZON_START, HORIZON_START + timedelta(days=30))
        assert result.truncated
        assert result.horizon_end == HORIZON_START + timedelta(hours=72)

    def test_does_not_claim_truncation_when_it_did_not_truncate(
        self, propagator: Propagator
    ) -> None:
        result = sweep([propagator], LISBON, HORIZON_START, HORIZON_END)
        assert not result.truncated
        assert result.horizon_end == HORIZON_END

    def test_horizon_exactly_at_the_limit_is_not_truncated(self, propagator: Propagator) -> None:
        # An off-by-one here would mark every maximum-length request truncated,
        # which is a lie the customer sees.
        limit = HORIZON_START + timedelta(hours=72)
        assert not sweep([propagator], LISBON, HORIZON_START, limit).truncated

    def test_windows_are_ordered_across_satellites(self) -> None:
        sets = parse_catalogue(SNAPSHOT.read_text())
        propagators = [Propagator(es) for es in sets]
        result = sweep(propagators, LISBON, HORIZON_START, HORIZON_END)
        assert result.satellites_evaluated == len(propagators)
        starts = [w.start for w in result.windows]
        assert starts == sorted(starts)

    def test_result_is_independent_of_propagator_order(self) -> None:
        # Two satellites can rise at the same instant. If the tie were broken by
        # list order, the sweep would return different results for the same
        # inputs depending on how the constellation was loaded — a determinism
        # break no single-satellite test would catch.
        sets = parse_catalogue(SNAPSHOT.read_text())
        forward = sweep([Propagator(es) for es in sets], LISBON, HORIZON_START, HORIZON_END)
        reversed_ = sweep(
            [Propagator(es) for es in reversed(sets)], LISBON, HORIZON_START, HORIZON_END
        )
        assert forward.windows == reversed_.windows

    @pytest.mark.parametrize("max_hours", [0.0, -1.0])
    def test_rejects_a_nonsensical_horizon_limit(self, max_hours: float) -> None:
        with pytest.raises(ValueError, match="must be positive"):
            HorizonPolicy(max_hours=max_hours)


class TestPerformance:
    def test_a_full_constellation_day_sweep_stays_within_budget(self) -> None:
        """Guards the consumer ack_wait assumption, not micro-performance.

        Measured on the development machine: 9 satellites over 24 hours takes
        roughly 0.5s at mid latitude and about 1s at 78N, where passes are far
        more frequent. The ceiling here is deliberately an order of magnitude
        above that — this exists to catch a change that makes the sweep ten
        times slower, not to fail whenever CI has a noisy neighbour.

        If this ever fires, the question is not "raise the ceiling" but "does
        ack_wait in M1-13 still hold".
        """
        sets = parse_catalogue(SNAPSHOT.read_text())
        propagators = [Propagator(es) for es in sets]
        started = time.perf_counter()
        result = sweep(propagators, LISBON, HORIZON_START, HORIZON_END)
        elapsed_s = time.perf_counter() - started

        assert result.window_count > 0
        assert elapsed_s < 10.0, (
            f"a 24h sweep of {len(propagators)} satellites took {elapsed_s:.1f}s; "
            "the consumer ack_wait assumption in M1-13 depends on this staying fast"
        )


class TestGridBoundary:
    """The inclusive final sample, which a comment claimed mattered and no test proved.

    Found by mutation: deleting the final `out.append(end)` in `_iterate` left
    the whole suite green. A justification with no test behind it is exactly the
    thing the M0 rules warn about, so these are the tests that make the claim
    real.
    """

    def test_a_clipped_window_ends_exactly_at_the_horizon(self, propagator: Propagator) -> None:
        # Cut a known pass in half at an instant that is NOT a whole number of
        # coarse steps from the horizon start. Without the final sample the grid
        # stops short and the window is reported as ending early — quietly
        # shorter than the geometry allows.
        first = search(propagator, LISBON, HORIZON_START, HORIZON_END)[0]
        start = first.start - timedelta(seconds=10)
        end = first.start + timedelta(seconds=130)  # 140s total, not a multiple of 60

        (window,) = search(propagator, LISBON, start, end)
        assert window.clipped_at_end
        assert window.end == end

    def test_a_window_opening_in_the_final_partial_step_is_found(
        self, propagator: Propagator
    ) -> None:
        # The rise happens between the last whole coarse sample and the horizon
        # end. If the grid stops at the last whole step, this pass does not
        # exist as far as the search is concerned.
        first = search(propagator, LISBON, HORIZON_START, HORIZON_END)[0]
        start = first.start - timedelta(seconds=100)
        end = first.start + timedelta(seconds=20)

        windows = search(propagator, LISBON, start, end)
        assert len(windows) == 1
        assert windows[0].clipped_at_end
