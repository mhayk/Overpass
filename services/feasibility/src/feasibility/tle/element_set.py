"""Two-line element sets: parsing, validation, and age classification.

Nothing here propagates anything. This module's whole job is to turn 69-column
fixed-format text into a value object we can trust, and to say how old it is.
SGP4 lives in M1-10 and depends on this being right first.

The reason it is its own module, with its own tests, against a frozen snapshot
of real element sets: a TLE that parses into the wrong epoch produces a
perfectly plausible access window that is wrong by minutes. There is no
exception, no log line, and no way to notice downstream — which makes this
exactly the kind of code CLAUDE.md means when it says reading is not
verification.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from enum import StrEnum

# Column spans, 0-indexed half-open, from the NORAD fixed-format definition.
# Written as named constants rather than inline slices because an off-by-one in
# a magic slice is invisible in review and catastrophic in effect.
_LINE_LENGTH = 69
_CATALOG_NUMBER = slice(2, 7)
_EPOCH = slice(18, 32)
_CHECKSUM = 68

# The pivot for two-digit years, fixed by the TLE format itself: 57-99 mean
# 1957-1999 (Sputnik onwards) and 00-56 mean 2000-2056. This is not a choice we
# get to make, and it is the single most common TLE parsing bug.
_YEAR_PIVOT = 57


class TleFormatError(ValueError):
    """A TLE could not be parsed, or failed its own integrity check."""


class Staleness(StrEnum):
    """Maps to sar.v1.schema.json#/$defs/TleStaleness."""

    FRESH = "FRESH"
    AGING = "AGING"
    STALE = "STALE"


@dataclass(frozen=True)
class StalenessPolicy:
    """Age thresholds, in hours.

    The contract calls these "configured thresholds" and deliberately does not
    fix them, so they are configuration with a default rather than a constant.

    The default — fresh under a day, stale past three — follows from how the
    data actually behaves: Celestrak publishes updated element sets for an
    active LEO satellite roughly daily, so anything older than that means we
    missed an update rather than that none was issued. Beyond about three days
    SGP4 along-track error on a LEO object grows past the point where a computed
    access window is worth acting on.

    THESE NUMBERS ARE AN ENGINEERING DEFAULT, NOT A MEASURED ONE. M1-12's
    golden-reference tests are what would turn them into a measured one, by
    showing where predicted and observed passes actually diverge. Until then
    they are a defensible starting point that is cheap to change, which is why
    they live here and not in the schema.
    """

    fresh_below_hours: float = 24.0
    stale_at_or_above_hours: float = 72.0

    def __post_init__(self) -> None:
        if not 0 < self.fresh_below_hours < self.stale_at_or_above_hours:
            msg = (
                "thresholds must satisfy 0 < fresh_below_hours < stale_at_or_above_hours, "
                f"got {self.fresh_below_hours} and {self.stale_at_or_above_hours}"
            )
            raise ValueError(msg)

    def classify(self, age_hours: float) -> Staleness:
        """Classify an age. Boundaries are closed downwards: exactly 24h is AGING."""
        if age_hours < 0:
            msg = f"age cannot be negative, got {age_hours}"
            raise ValueError(msg)
        if age_hours < self.fresh_below_hours:
            return Staleness.FRESH
        if age_hours < self.stale_at_or_above_hours:
            return Staleness.AGING
        return Staleness.STALE


@dataclass(frozen=True)
class ElementSet:
    """One validated two-line element set."""

    name: str
    norad_id: int
    epoch: datetime
    line1: str
    line2: str

    def age_hours(self, at: datetime) -> float:
        """Hours between this element set's epoch and `at`.

        Negative ages are possible and are not an error here: an element set
        whose epoch is in the future relative to the query is a real thing that
        happens with predicted ephemerides, and the caller decides what it
        means. `StalenessPolicy.classify` is the one that refuses.
        """
        if at.tzinfo is None:
            msg = "refusing to compute an age against a naive datetime"
            raise ValueError(msg)
        return (at - self.epoch).total_seconds() / 3600.0

    def staleness(self, at: datetime, policy: StalenessPolicy) -> Staleness:
        return policy.classify(self.age_hours(at))


def tle_checksum(line: str) -> int:
    """The mod-10 checksum of a TLE line, over its first 68 columns.

    Digits count as themselves, a minus sign counts as 1, and everything else
    — letters, spaces, decimal points, plus signs — counts as zero.
    """
    total = 0
    for char in line[:_CHECKSUM]:
        if char.isdigit():
            total += int(char)
        elif char == "-":
            total += 1
    return total % 10


def parse_epoch(field: str) -> datetime:
    """Decode a TLE epoch field (`YYDDD.DDDDDDDD`) into a UTC datetime.

    The fractional part is a fraction of a day, and day-of-year is 1-based, so
    001.0 is midnight on 1 January. Treating it as 0-based puts every result a
    day early, which is the second most common TLE parsing bug after the year
    pivot.
    """
    field = field.strip()
    try:
        two_digit_year = int(field[:2])
        day_of_year = float(field[2:])
    except ValueError as exc:
        msg = f"malformed epoch field {field!r}"
        raise TleFormatError(msg) from exc

    if not math.isfinite(day_of_year) or day_of_year < 1.0:
        msg = f"epoch day-of-year must be >= 1.0, got {day_of_year}"
        raise TleFormatError(msg)

    year = 1900 + two_digit_year if two_digit_year >= _YEAR_PIVOT else 2000 + two_digit_year
    return datetime(year, 1, 1, tzinfo=UTC) + timedelta(days=day_of_year - 1.0)


def parse(name: str, line1: str, line2: str, *, verify_checksum: bool = True) -> ElementSet:
    """Parse and validate one element set.

    Raises TleFormatError rather than returning a partial object. A half-parsed
    TLE is worse than none: it propagates into an access window that looks fine.
    """
    for label, line in (("line 1", line1), ("line 2", line2)):
        if len(line) != _LINE_LENGTH:
            msg = f"{label} is {len(line)} characters, expected {_LINE_LENGTH}"
            raise TleFormatError(msg)

    if not line1.startswith("1 ") or not line2.startswith("2 "):
        msg = "lines are not numbered 1 and 2"
        raise TleFormatError(msg)

    if verify_checksum:
        for label, line in (("line 1", line1), ("line 2", line2)):
            expected = tle_checksum(line)
            try:
                actual = int(line[_CHECKSUM])
            except ValueError as exc:
                msg = f"{label} checksum column is not a digit"
                raise TleFormatError(msg) from exc
            if actual != expected:
                msg = f"{label} checksum is {actual}, computed {expected}"
                raise TleFormatError(msg)

    try:
        norad_1 = int(line1[_CATALOG_NUMBER])
        norad_2 = int(line2[_CATALOG_NUMBER])
    except ValueError as exc:
        msg = "catalog number is not numeric"
        raise TleFormatError(msg) from exc

    # Two lines from different satellites, spliced together by a bad fetch or a
    # careless edit, would otherwise propagate one satellite's orbit under
    # another's identity.
    if norad_1 != norad_2:
        msg = f"catalog numbers disagree: line 1 says {norad_1}, line 2 says {norad_2}"
        raise TleFormatError(msg)

    return ElementSet(
        name=name.strip(),
        norad_id=norad_1,
        epoch=parse_epoch(line1[_EPOCH]),
        line1=line1,
        line2=line2,
    )


def parse_catalogue(text: str, *, verify_checksum: bool = True) -> list[ElementSet]:
    """Parse a three-line-per-object TLE catalogue.

    Blank lines and `#` comments are skipped, so a committed snapshot can carry
    a header explaining what it is and why it must not be casually regenerated.
    """
    lines = [
        line.rstrip("\r\n")
        for line in text.splitlines()
        if line.strip() and not line.lstrip().startswith("#")
    ]

    if len(lines) % 3 != 0:
        msg = f"catalogue has {len(lines)} lines, not a multiple of three"
        raise TleFormatError(msg)

    out: list[ElementSet] = []
    for i in range(0, len(lines), 3):
        name, line1, line2 = lines[i], lines[i + 1], lines[i + 2]
        try:
            out.append(parse(name, line1, line2, verify_checksum=verify_checksum))
        except TleFormatError as exc:
            msg = f"object {i // 3 + 1} ({name.strip()!r}): {exc}"
            raise TleFormatError(msg) from exc
    return out
