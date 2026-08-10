"""The execution model, tested without a broker, a database or a clock."""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from random import Random

import pytest

from simulator.execution import (
    INJECTABLE,
    FailureReason,
    GroundPoint,
    InjectionRates,
    Status,
    coverage_ratio,
    decide,
    drift_actual_window,
    great_circle_km,
)

WINDOW = (
    datetime(2026, 8, 11, 12, 0, tzinfo=UTC),
    datetime(2026, 8, 11, 12, 0, 15, tzinfo=UTC),
)

# Swath half-widths from reference.satellites' sensor_modes, which is where the
# real numbers live. SPOTLIGHT is the narrow one and therefore the interesting
# one — it is what a stale element set costs you first.
SPOTLIGHT_HALF_KM = 2.5
SCAN_HALF_KM = 50.0


def never_injects() -> InjectionRates:
    """Rates that isolate the computed path from the rolled one."""
    return InjectionRates(
        attitude_error=0.0,
        slew_overrun=0.0,
        power_budget_exceeded=0.0,
        thermal_limit=0.0,
        sensor_fault=0.0,
        ground_abort=0.0,
        partial=0.0,
    )


class TestCoverageRatio:
    def test_inside_the_swath_is_full_coverage(self) -> None:
        assert coverage_ratio(1.0, SPOTLIGHT_HALF_KM) == 1.0
        assert coverage_ratio(SPOTLIGHT_HALF_KM, SPOTLIGHT_HALF_KM) == 1.0

    def test_a_full_swath_width_out_is_nothing(self) -> None:
        assert coverage_ratio(SPOTLIGHT_HALF_KM * 2, SPOTLIGHT_HALF_KM) == 0.0

    def test_it_degrades_rather_than_stepping(self) -> None:
        # The edge is the interesting case: a target just outside a narrow swath
        # is a partial collection someone might accept, and a step function
        # would report it as a total loss.
        clipped = coverage_ratio(SPOTLIGHT_HALF_KM * 1.5, SPOTLIGHT_HALF_KM)
        assert 0.0 < clipped < 1.0

    def test_it_never_goes_negative(self) -> None:
        assert coverage_ratio(1_000.0, SPOTLIGHT_HALF_KM) == 0.0

    def test_a_nonsense_swath_is_refused(self) -> None:
        with pytest.raises(ValueError, match="swath half-width must be positive"):
            coverage_ratio(1.0, 0.0)


class TestTheComputedOutcome:
    def test_a_target_outside_the_swath_is_a_drift_miss(self) -> None:
        outcome = decide(
            cross_track_km=29.1,  # CAPELLA-13's measured drift
            swath_half_km=SPOTLIGHT_HALF_KM,
            planning_tle_age_hours=98.0,
            scheduled_window=WINDOW,
            rates=never_injects(),
            random=Random(1),
        )
        assert outcome.status is Status.FAILED
        assert outcome.failure_reason is FailureReason.TLE_DRIFT_MISS
        # Null for a collection that never happened. The contract says so and
        # the read model distinguishes the two.
        assert outcome.actual_window is None

    def test_the_same_drift_succeeds_on_a_wider_swath(self) -> None:
        # THE PROPERTY THE WHOLE DESIGN EXISTS TO PRODUCE. The same satellite,
        # the same pass, the same error — and SCAN collects while SPOTLIGHT does
        # not. Nobody configured that; it is what the geometry does, and it is
        # the visible consequence of a loose staleness threshold.
        spotlight = decide(
            cross_track_km=29.1,
            swath_half_km=SPOTLIGHT_HALF_KM,
            planning_tle_age_hours=98.0,
            scheduled_window=WINDOW,
            rates=never_injects(),
            random=Random(1),
        )
        scan = decide(
            cross_track_km=29.1,
            swath_half_km=SCAN_HALF_KM,
            planning_tle_age_hours=98.0,
            scheduled_window=WINDOW,
            rates=never_injects(),
            random=Random(1),
        )

        assert spotlight.status is Status.FAILED
        assert scan.status is Status.SUCCEEDED

    def test_a_clipped_collection_is_partial_not_failed(self) -> None:
        outcome = decide(
            cross_track_km=SPOTLIGHT_HALF_KM * 1.5,
            swath_half_km=SPOTLIGHT_HALF_KM,
            planning_tle_age_hours=98.0,
            scheduled_window=WINDOW,
            rates=never_injects(),
            random=Random(1),
        )
        assert outcome.status is Status.PARTIAL
        assert outcome.failure_reason is FailureReason.TLE_DRIFT_MISS
        # Something was collected, so there IS a window — the difference between
        # PARTIAL and FAILED that the read model has to be able to see.
        assert outcome.actual_window == WINDOW
        assert 0.0 < outcome.target_coverage_ratio < 1.0

    def test_a_pass_inside_the_swath_succeeds(self) -> None:
        outcome = decide(
            cross_track_km=0.4,  # SENTINEL-1B's measured drift
            swath_half_km=SPOTLIGHT_HALF_KM,
            planning_tle_age_hours=90.0,
            scheduled_window=WINDOW,
            rates=never_injects(),
            random=Random(1),
        )
        assert outcome.status is Status.SUCCEEDED
        assert outcome.failure_reason is None
        assert outcome.target_coverage_ratio == 1.0

    def test_the_margin_is_reported_even_on_success(self) -> None:
        # "We made it by 400 metres" and "we made it by 40 km" are different
        # facts about the same SUCCEEDED, and only one of them should let anyone
        # relax about the staleness threshold.
        outcome = decide(
            cross_track_km=2.4,
            swath_half_km=SPOTLIGHT_HALF_KM,
            planning_tle_age_hours=90.0,
            scheduled_window=WINDOW,
            rates=never_injects(),
            random=Random(1),
        )
        assert outcome.status is Status.SUCCEEDED
        assert outcome.cross_track_km == 2.4
        assert outcome.planning_tle_age_hours == 90.0


class TestComputedBeatsInjected:
    def test_a_geometric_miss_is_never_relabelled(self) -> None:
        # ORDERING, AND IT IS EASY TO BREAK IN A REFACTOR. With every injected
        # failure at certainty, a genuine drift miss must still be reported as a
        # drift miss — otherwise the one outcome this system can defend gets
        # overwritten by a coin flip that rolled first.
        certain = InjectionRates(
            attitude_error=1.0,
            slew_overrun=0.0,
            power_budget_exceeded=0.0,
            thermal_limit=0.0,
            sensor_fault=0.0,
            ground_abort=0.0,
            partial=1.0,
        )
        outcome = decide(
            cross_track_km=223.8,  # SENTINEL-1A's measured drift
            swath_half_km=SPOTLIGHT_HALF_KM,
            planning_tle_age_hours=123.0,
            scheduled_window=WINDOW,
            rates=certain,
            random=Random(7),
        )
        assert outcome.failure_reason is FailureReason.TLE_DRIFT_MISS


class TestInjection:
    def test_a_certain_failure_happens(self) -> None:
        rates = InjectionRates(
            attitude_error=0.0,
            slew_overrun=0.0,
            power_budget_exceeded=0.0,
            thermal_limit=0.0,
            sensor_fault=1.0,
            ground_abort=0.0,
            partial=0.0,
        )
        outcome = decide(
            cross_track_km=0.1,
            swath_half_km=SCAN_HALF_KM,
            planning_tle_age_hours=10.0,
            scheduled_window=WINDOW,
            rates=rates,
            random=Random(3),
        )
        assert outcome.status is Status.FAILED
        assert outcome.failure_reason is FailureReason.SENSOR_FAULT

    def test_a_ground_abort_means_it_never_started(self) -> None:
        rates = InjectionRates(
            attitude_error=0.0,
            slew_overrun=0.0,
            power_budget_exceeded=0.0,
            thermal_limit=0.0,
            sensor_fault=0.0,
            ground_abort=1.0,
            partial=0.0,
        )
        outcome = decide(
            cross_track_km=0.1,
            swath_half_km=SCAN_HALF_KM,
            planning_tle_age_hours=10.0,
            scheduled_window=WINDOW,
            rates=rates,
            random=Random(3),
        )
        assert outcome.status is Status.SKIPPED
        assert outcome.actual_window is None

    def test_the_same_seed_gives_the_same_run(self) -> None:
        # A surprising demo has to be reproducible, which is why the seed is
        # configuration and is logged at startup.
        def run(seed: int) -> list[tuple[Status, FailureReason | None]]:
            random = Random(seed)
            return [
                (o.status, o.failure_reason)
                for o in (
                    decide(
                        cross_track_km=0.1,
                        swath_half_km=SCAN_HALF_KM,
                        planning_tle_age_hours=10.0,
                        scheduled_window=WINDOW,
                        rates=InjectionRates(),
                        random=random,
                    )
                    for _ in range(200)
                )
            ]

        assert run(42) == run(42)
        assert run(42) != run(43)

    def test_every_injectable_reason_can_actually_occur(self) -> None:
        # A failure reason nobody can produce is a code path nobody has run, and
        # the read model handling a committed acquisition that then failed is
        # the path most demo systems never execute.
        for reason in INJECTABLE:
            rates = InjectionRates(
                **{
                    field: (1.0 if field == reason.value.lower() else 0.0)
                    for field in InjectionRates().as_map()
                }
            )
            outcome = decide(
                cross_track_km=0.1,
                swath_half_km=SCAN_HALF_KM,
                planning_tle_age_hours=10.0,
                scheduled_window=WINDOW,
                rates=rates,
                random=Random(5),
            )
            assert outcome.failure_reason is reason

    def test_a_rate_outside_zero_to_one_is_refused(self) -> None:
        with pytest.raises(ValueError, match="must be in"):
            InjectionRates(attitude_error=1.5)

    def test_rates_that_leave_no_successes_are_refused(self) -> None:
        with pytest.raises(ValueError, match="leaves no successes"):
            InjectionRates(
                attitude_error=0.5,
                slew_overrun=0.5,
                power_budget_exceeded=0.5,
                thermal_limit=0.0,
                sensor_fault=0.0,
                ground_abort=0.0,
            )


class TestGeometryHelpers:
    def test_great_circle_matches_a_known_distance(self) -> None:
        # Rotterdam to Hamburg, ~365 km. A golden value rather than a
        # reimplementation of the formula, which would only prove the test can
        # copy the code.
        rotterdam = GroundPoint(51.9, 4.4)
        hamburg = GroundPoint(53.55, 9.9)
        assert great_circle_km(rotterdam, hamburg) == pytest.approx(415, abs=15)

    def test_a_point_is_zero_from_itself(self) -> None:
        p = GroundPoint(51.9, 4.4)
        assert great_circle_km(p, p) == 0.0

    def test_the_actual_window_shifts_with_the_miss(self) -> None:
        # One cause, two visible effects: the satellite that was not where the
        # propagation said also did not cross when it was expected to.
        on_target = drift_actual_window(WINDOW, 0.0)
        assert on_target == WINDOW

        drifted = drift_actual_window(WINDOW, 75.0)
        assert drifted[0] > WINDOW[0]
        assert drifted[1] - drifted[0] == WINDOW[1] - WINDOW[0]

    def test_the_shift_is_capped(self) -> None:
        # A hundred-kilometre miss must not report an acquisition a minute early
        # and make the timeline unreadable.
        wild = drift_actual_window(WINDOW, 100_000.0)
        assert wild[0] - WINDOW[0] <= timedelta(seconds=20)
