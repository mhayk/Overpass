"""Reference data the sweep reads but never writes: instrument parameters.

The other half of what a sweep needs about a satellite. `tle.store` reads the
element set; this reads the sensor modes. Two functions and two queries against
the same table rather than one, deliberately: the ephemeris sweep needs the
element set and never the modes, and a satellite whose `sensor_modes` blob is
malformed must not be able to stop an orbit being drawn.

A MALFORMED MODE IS AN ERROR, NOT A SKIP. `reference.satellites.sensor_modes` is
one of the four JSONB cases ADR-0004 permits, which means Postgres CHECKs that
it is an object and nothing more — not the keys, not the values, not the
invariants `SensorMode` holds. Swallowing a bad one would remove a satellite
from the constellation silently, and the sweep would report honest-looking
NO_ACCESS_IN_HORIZON for a target that satellite could see.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from feasibility.sar import ImagingMode, LookSide, SensorMode

if TYPE_CHECKING:
    from collections.abc import Sequence

    import psycopg


def sensor_modes(
    connection: psycopg.Connection[Any], satellite_ids: Sequence[str] | None = None
) -> dict[str, dict[str, SensorMode]]:
    """Per-satellite imaging modes, keyed by satellite id and then mode name.

    `satellite_ids` narrows the query; None reads the whole constellation, which
    is what a sweep wants — the set of satellites worth evaluating is decided by
    the element sets and the customer's exclusions, not here.
    """
    where, params = "", []
    if satellite_ids is not None:
        where = "WHERE satellite_id = ANY(%s)"
        params = [list(satellite_ids)]

    with connection.cursor() as cursor:
        cursor.execute(
            f"SELECT satellite_id, sensor_modes FROM reference.satellites {where}",
            params,
        )
        rows = cursor.fetchall()

    return {
        satellite_id: {
            name: _mode_from(satellite_id, name, parameters)
            for name, parameters in (blob or {}).items()
        }
        for satellite_id, blob in rows
    }


def _mode_from(satellite_id: str, name: str, parameters: dict[str, Any]) -> SensorMode:
    """One SensorMode, or a ValueError naming the satellite that carries it.

    The satellite id is in the message because the alternative — "incidence band
    must satisfy 0 <= min < max" with no subject — is a message that sends
    somebody reading every row of a seed file.
    """
    if name not in {m.value for m in ImagingMode}:
        msg = (
            f"{satellite_id} declares an imaging mode {name!r} that the contract does not "
            f"have. sar.v1 permits: {', '.join(sorted(m.value for m in ImagingMode))}"
        )
        raise ValueError(msg)

    try:
        sides = parameters.get("permitted_look_sides") or ["LEFT", "RIGHT"]
        return SensorMode(
            mode=ImagingMode(name),
            swath_width_km=float(parameters["swath_width_km"]),
            resolution_m=float(parameters["resolution_m"]),
            min_dwell_s=float(parameters["min_dwell_s"]),
            max_dwell_s=float(parameters["max_dwell_s"]),
            **_optional(parameters, "min_incidence_deg"),
            **_optional(parameters, "max_incidence_deg"),
            **_optional(parameters, "max_squint_deg"),
            permitted_look_sides=tuple(LookSide(side) for side in sides),
        )
    except (KeyError, TypeError, ValueError) as exc:
        msg = f"{satellite_id} has an unusable {name} sensor mode: {exc}"
        raise ValueError(msg) from exc


def _optional(parameters: dict[str, Any], key: str) -> dict[str, float]:
    """Pass a parameter through only when it is present.

    SensorMode's defaults come from the contract's own description text and are
    stated once, in that dataclass. Passing None here would override a default
    with nothing rather than leave it alone.
    """
    value = parameters.get(key)
    return {} if value is None else {key: float(value)}
