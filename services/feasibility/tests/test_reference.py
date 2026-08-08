"""Reading the constellation's instrument parameters out of reference data.

The sweep needs two things per satellite and they live in two different places:
the element set, which `tle.store` reads, and the sensor modes, which are here.
Split rather than fetched together on purpose — the ephemeris sweep needs the
first and never the second, and a malformed `sensor_modes` blob should not be
able to stop an orbit being drawn.

What makes this worth its own module rather than a dict comprehension: the JSONB
column is shaped by `sar.v1.schema.json#/$defs/SensorModeParameters`, and
`SensorMode` has invariants of its own — an incidence band that is not ordered,
a dwell band that is not, a mode with no permitted look side. Seed data that
violates one of those must fail loudly here rather than produce a sensor that
silently accepts everything.
"""

from __future__ import annotations

import json
import os
from typing import TYPE_CHECKING, Any

import psycopg
import pytest

from feasibility.reference import sensor_modes
from feasibility.sar import LookSide

if TYPE_CHECKING:
    from collections.abc import Iterator

DSN = os.environ.get("OVERPASS_TEST_DSN")

pytestmark = pytest.mark.skipif(
    not DSN, reason="set OVERPASS_TEST_DSN to run the reference-data tests"
)


def dsn() -> str:
    assert DSN is not None
    return DSN


@pytest.fixture
def connection() -> Iterator[psycopg.Connection[Any]]:
    """Rolled back, so a test that inserts a satellite does not leave one."""
    with psycopg.connect(dsn()) as conn, conn.transaction() as transaction:
        yield conn
        transaction.force_rollback = True


class TestTheSeededConstellation:
    def test_every_satellite_has_the_three_imaging_modes(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        modes = sensor_modes(connection)
        assert modes, "the constellation is not seeded — run `make seed`"

        for satellite_id, by_mode in modes.items():
            assert set(by_mode) == {"SPOTLIGHT", "STRIPMAP", "SCAN"}, satellite_id

    def test_the_parameters_are_the_ones_the_seeder_wrote(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        by_mode = next(iter(sensor_modes(connection).values()))

        spotlight = by_mode["SPOTLIGHT"]
        assert spotlight.resolution_m == 0.5
        assert spotlight.swath_width_km == 5.0
        assert spotlight.min_dwell_s == 8.0

        # SCAN is right-looking only in the seed. A mode that quietly gained
        # LEFT would double the access this system believes it has.
        assert by_mode["SCAN"].permitted_look_sides == (LookSide.RIGHT,)

    def test_the_trade_between_resolution_and_swath_survives(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # The property that makes one request contend with another. If the three
        # modes stopped trading resolution against swath they would collapse
        # into one mode with three names, and the demo would show nothing.
        by_mode = next(iter(sensor_modes(connection).values()))
        assert (
            by_mode["SPOTLIGHT"].resolution_m
            < by_mode["STRIPMAP"].resolution_m
            < by_mode["SCAN"].resolution_m
        )
        assert (
            by_mode["SPOTLIGHT"].swath_width_km
            < by_mode["STRIPMAP"].swath_width_km
            < by_mode["SCAN"].swath_width_km
        )


class TestWhatItRefuses:
    def test_seed_data_that_violates_a_sensor_invariant_fails_loudly(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # An incidence band with min above max. SensorMode refuses to construct
        # one; the point of this test is that the refusal is not swallowed here
        # into a satellite that is silently absent from the sweep.
        _insert_satellite(
            connection,
            "BADSENSOR-1",
            {
                "STRIPMAP": {
                    "mode": "STRIPMAP",
                    "resolution_m": 1.0,
                    "swath_width_km": 30.0,
                    "min_dwell_s": 10.0,
                    "max_dwell_s": 60.0,
                    "min_incidence_deg": 60.0,
                    "max_incidence_deg": 20.0,
                    "permitted_look_sides": ["LEFT", "RIGHT"],
                }
            },
        )

        with pytest.raises(ValueError, match="BADSENSOR-1"):
            sensor_modes(connection)

    def test_a_mode_name_outside_the_contract_is_refused(
        self, connection: psycopg.Connection[Any]
    ) -> None:
        # reference.satellites CHECKs nothing about the KEYS of the jsonb blob,
        # so an invented mode reaches here. Accepting it would put a mode on an
        # opportunity that the contract's ImagingMode enum rejects, and the
        # event would be unpublishable at the very end of a sweep.
        _insert_satellite(
            connection,
            "BADMODE-1",
            {
                "TELESCOPE": {
                    "mode": "TELESCOPE",
                    "resolution_m": 1.0,
                    "swath_width_km": 30.0,
                    "min_dwell_s": 10.0,
                    "max_dwell_s": 60.0,
                    "permitted_look_sides": ["RIGHT"],
                }
            },
        )

        with pytest.raises(ValueError, match="TELESCOPE"):
            sensor_modes(connection)


def _insert_satellite(
    connection: psycopg.Connection[Any], satellite_id: str, modes: dict[str, Any]
) -> None:
    with connection.cursor() as cursor:
        cursor.execute(
            """
            INSERT INTO reference.satellites
                (satellite_id, norad_id, display_name, sensor_modes, duty_cycle_budget_s)
            VALUES (%s, %s, %s, %s::jsonb, 600)
            """,
            (satellite_id, 999001, satellite_id, json.dumps(modes)),
        )
