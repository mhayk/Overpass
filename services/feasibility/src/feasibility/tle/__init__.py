"""TLE ingestion: fetch, parse, validate, and classify by age."""

from feasibility.tle.celestrak import CelestrakClient, CelestrakError
from feasibility.tle.element_set import (
    ElementSet,
    Staleness,
    StalenessPolicy,
    TleFormatError,
    parse,
    parse_catalogue,
    parse_epoch,
    tle_checksum,
)

__all__ = [
    "CelestrakClient",
    "CelestrakError",
    "ElementSet",
    "Staleness",
    "StalenessPolicy",
    "TleFormatError",
    "parse",
    "parse_catalogue",
    "parse_epoch",
    "tle_checksum",
]
