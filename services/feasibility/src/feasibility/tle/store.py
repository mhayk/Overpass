"""Reading element sets out of `reference.tle_sets`.

`celestrak.py` fetches element sets from the network and the seeder writes them
here; this is the other end, and until the ephemeris sweep there was nothing in
the service that read them back. ADR-0011 is why the two paths exist at all.

THE SATELLITE ID COMES FROM THE COLUMN, NOT FROM THE ELEMENT SET NAME. They are
different strings and it matters: `reference.satellites` enforces
`^[A-Z0-9][A-Z0-9_-]{0,31}$`, and Celestrak names like `CAPELLA-11 (ACADIA-1)`
do not satisfy it — the seeder strips the parenthetical to derive the id. An
event carrying the name instead of the id would reference a satellite that
exists nowhere else in the system, and would do it silently.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import TYPE_CHECKING, Any

from feasibility.tle.element_set import ElementSet, parse

if TYPE_CHECKING:
    from datetime import datetime

    import psycopg


@dataclass(frozen=True)
class SatelliteElementSet:
    """One satellite's element set, under the id the rest of the system uses."""

    satellite_id: str
    display_name: str
    element_set: ElementSet


def newest_element_sets(
    connection: psycopg.Connection[Any], at: datetime
) -> list[SatelliteElementSet]:
    """The newest element set at or before `at`, one per satellite.

    `epoch <= at` rather than simply the newest row. An element set whose epoch
    is in the future relative to the instant being propagated is a prediction
    made after the fact, and it would win every ordering while being the wrong
    answer to the question asked.

    Checksums are re-verified on the way out, by `parse`. The database CHECKs
    the line length and the seeder decoded the epoch, but nothing has ever
    asserted that the two lines still agree with themselves — and a corrupted
    line propagates into a plausible, wrong orbit rather than an error.
    """
    if at.tzinfo is None:
        msg = "refusing to select element sets against a naive datetime"
        raise ValueError(msg)

    with connection.cursor() as cursor:
        cursor.execute(
            """
            SELECT DISTINCT ON (t.satellite_id)
                   t.satellite_id, s.display_name, t.line1, t.line2, t.epoch
            FROM reference.tle_sets t
            JOIN reference.satellites s ON s.satellite_id = t.satellite_id
            WHERE t.epoch <= %s
            ORDER BY t.satellite_id, t.epoch DESC
            """,
            (at,),
        )
        rows = cursor.fetchall()

    out: list[SatelliteElementSet] = []
    for satellite_id, display_name, line1, line2, stored_epoch in rows:
        # Parsed from the LINES, not taken from the column. The lines are the
        # record and the column is a decode of them — SGP4 will read the lines
        # whatever the column says.
        element_set = parse(display_name, line1, line2)
        _refuse_disagreeing_epoch(satellite_id, element_set, stored_epoch)
        out.append(
            SatelliteElementSet(
                satellite_id=satellite_id,
                display_name=display_name,
                element_set=element_set,
            )
        )
    return out


# The column is decoded from the line, so any difference at all is corruption
# rather than rounding. A second of slack absorbs a storage round-trip and
# nothing else.
_EPOCH_AGREEMENT_S = 1.0


def _refuse_disagreeing_epoch(
    satellite_id: str, element_set: ElementSet, stored_epoch: datetime
) -> None:
    """Refuse a row whose epoch column contradicts its own element set.

    This is not defensive padding. The query ORDERS BY the column and this
    function's caller REPORTS the decoded value, so a row where the two disagree
    is one that sorts as new and propagates as old — and the resulting track
    would carry an event id derived from an epoch that is not the one it was
    computed from. Silently preferring either value would make the provenance
    field on the published event a lie, which is the one thing that field is for.
    """
    drift_s = abs((element_set.epoch - stored_epoch).total_seconds())
    if drift_s > _EPOCH_AGREEMENT_S:
        msg = (
            f"reference.tle_sets row for {satellite_id} is inconsistent: the epoch column "
            f"says {stored_epoch.isoformat()} and line 1 decodes to "
            f"{element_set.epoch.isoformat()}, {drift_s:.1f} s apart. The row sorts by the "
            "column and would propagate from the line, so this cannot be resolved here."
        )
        raise ValueError(msg)
