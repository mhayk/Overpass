"""Tests for TLE parsing, against real element sets and an independent oracle.

The expected epochs here are NOT produced by running `parse_epoch` and pasting
the answer. That would be a snapshot of whatever the code does, and it would
pass forever with the year pivot inverted or the day-of-year off by one. They
come from `datetime.strptime` with `%Y %j`, which is a separate implementation
of the same calendar arithmetic in the standard library.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest

from feasibility.tle import (
    Staleness,
    StalenessPolicy,
    TleFormatError,
    parse,
    parse_catalogue,
    parse_epoch,
    tle_checksum,
)


# Frozen snapshot, committed under testdata/. Located by walking up rather than
# by a relative hop count, so moving this file does not silently break it.
def _repo_root() -> Path:
    here = Path(__file__).resolve()
    for parent in here.parents:
        if (parent / "testdata").is_dir() and (parent / "contracts").is_dir():
            return parent
    msg = "could not locate the repository root from the test file"
    raise RuntimeError(msg)


SNAPSHOT = _repo_root() / "testdata" / "tle" / "sar-constellation.2026-08-07.tle"

# Sentinel-1A, exactly as committed in the snapshot.
S1A_NAME = "SENTINEL-1A"
S1A_L1 = "1 39634U 14016A   26217.94446112  .00000085  00000+0  27560-4 0  9992"
S1A_L2 = "2 39634  98.1585 224.7210 0001387  84.4412 275.6946 14.59278904657278"


def oracle_epoch(year: int, day_of_year: float) -> datetime:
    """Independent epoch computation via strptime, not via parse_epoch."""
    whole = int(day_of_year)
    fraction = day_of_year - whole
    midnight = datetime.strptime(f"{year} {whole}", "%Y %j").replace(tzinfo=UTC)
    return midnight + timedelta(days=fraction)


class TestChecksum:
    def test_real_lines_carry_a_valid_checksum(self) -> None:
        assert tle_checksum(S1A_L1) == int(S1A_L1[68])
        assert tle_checksum(S1A_L2) == int(S1A_L2[68])

    def test_minus_sign_counts_as_one(self) -> None:
        # "27560-4" in line 1 contributes that minus. Flipping it to a plus must
        # change the checksum, or the sign is not being counted at all.
        flipped = S1A_L1.replace("27560-4", "27560+4")
        assert tle_checksum(flipped) != tle_checksum(S1A_L1)

    def test_every_object_in_the_snapshot_validates(self) -> None:
        sets = parse_catalogue(SNAPSHOT.read_text(), verify_checksum=True)
        assert len(sets) == 9
        for es in sets:
            assert tle_checksum(es.line1) == int(es.line1[68])
            assert tle_checksum(es.line2) == int(es.line2[68])


class TestEpoch:
    def test_sentinel_1a_epoch_matches_the_oracle(self) -> None:
        assert parse_epoch("26217.94446112") == oracle_epoch(2026, 217.94446112)

    def test_day_of_year_is_one_based(self) -> None:
        # 001.0 is midnight on 1 January, not 2 January. Treating the field as
        # zero-based puts every epoch a day early and every access window with
        # it.
        assert parse_epoch("26001.00000000") == datetime(2026, 1, 1, tzinfo=UTC)

    @pytest.mark.parametrize(
        ("field", "expected_year"),
        [
            ("57001.00000000", 1957),  # pivot, low side — Sputnik
            ("99001.00000000", 1999),
            ("00001.00000000", 2000),
            ("56001.00000000", 2056),  # pivot, high side
        ],
    )
    def test_two_digit_year_pivot(self, field: str, expected_year: int) -> None:
        assert parse_epoch(field).year == expected_year

    def test_fractional_day_is_a_fraction_of_a_day(self) -> None:
        # Half a day past the start of 1 January.
        assert parse_epoch("26001.50000000") == datetime(2026, 1, 1, 12, tzinfo=UTC)

    def test_epoch_is_timezone_aware_utc(self) -> None:
        assert parse_epoch("26217.94446112").tzinfo is UTC

    @pytest.mark.parametrize("field", ["26000.00000000", "26-01.0000000", "abcde.fghijklm"])
    def test_rejects_malformed_epochs(self, field: str) -> None:
        with pytest.raises(TleFormatError):
            parse_epoch(field)


class TestParse:
    def test_parses_a_real_element_set(self) -> None:
        es = parse(S1A_NAME, S1A_L1, S1A_L2)
        assert es.name == "SENTINEL-1A"
        assert es.norad_id == 39634
        assert es.epoch == oracle_epoch(2026, 217.94446112)

    def test_rejects_a_corrupted_digit(self) -> None:
        # A single transposed digit is what a bad copy-paste looks like, and the
        # checksum is the only thing standing between it and a wrong orbit.
        corrupted = S1A_L1[:20] + ("8" if S1A_L1[20] != "8" else "7") + S1A_L1[21:]
        with pytest.raises(TleFormatError, match="checksum"):
            parse(S1A_NAME, corrupted, S1A_L2)

    def test_rejects_lines_from_different_satellites(self) -> None:
        other_l2 = "2 41456  98.0872 282.0329 0012819 325.0618  34.9762 14.99352045551440"
        with pytest.raises(TleFormatError, match="catalog numbers disagree"):
            parse(S1A_NAME, S1A_L1, other_l2)

    def test_rejects_wrong_length(self) -> None:
        with pytest.raises(TleFormatError, match="expected 69"):
            parse(S1A_NAME, S1A_L1[:-1], S1A_L2)

    def test_rejects_swapped_lines(self) -> None:
        with pytest.raises(TleFormatError, match="numbered"):
            parse(S1A_NAME, S1A_L2, S1A_L1)


class TestCatalogue:
    def test_skips_comments_and_blank_lines(self) -> None:
        text = f"# a header\n\n{S1A_NAME}\n{S1A_L1}\n{S1A_L2}\n"
        assert len(parse_catalogue(text)) == 1

    def test_rejects_a_truncated_catalogue(self) -> None:
        text = f"{S1A_NAME}\n{S1A_L1}\n"
        with pytest.raises(TleFormatError, match="multiple of three"):
            parse_catalogue(text)

    def test_names_the_offending_object(self) -> None:
        bad = S1A_L2[:-1] + ("0" if S1A_L2[-1] != "0" else "1")
        text = f"{S1A_NAME}\n{S1A_L1}\n{bad}\n"
        with pytest.raises(TleFormatError, match="SENTINEL-1A"):
            parse_catalogue(text)


class TestStaleness:
    @pytest.mark.parametrize(
        ("age", "expected"),
        [
            (0.0, Staleness.FRESH),
            (23.999, Staleness.FRESH),
            (24.0, Staleness.AGING),  # boundary is closed downwards
            (71.999, Staleness.AGING),
            (72.0, Staleness.STALE),
            (1000.0, Staleness.STALE),
        ],
    )
    def test_boundaries(self, age: float, expected: Staleness) -> None:
        assert StalenessPolicy().classify(age) == expected

    def test_negative_age_is_refused(self) -> None:
        with pytest.raises(ValueError, match="negative"):
            StalenessPolicy().classify(-0.1)

    @pytest.mark.parametrize(
        ("fresh", "stale"),
        [(0.0, 72.0), (-1.0, 72.0), (72.0, 72.0), (100.0, 72.0)],
    )
    def test_incoherent_thresholds_are_refused(self, fresh: float, stale: float) -> None:
        with pytest.raises(ValueError, match="thresholds must satisfy"):
            StalenessPolicy(fresh_below_hours=fresh, stale_at_or_above_hours=stale)

    def test_age_against_a_known_instant(self) -> None:
        es = parse(S1A_NAME, S1A_L1, S1A_L2)
        at = es.epoch + timedelta(hours=30)
        assert es.age_hours(at) == pytest.approx(30.0)
        assert es.staleness(at, StalenessPolicy()) is Staleness.AGING

    def test_naive_datetime_is_refused(self) -> None:
        # A naive datetime here would be silently treated as local time by any
        # arithmetic that accepted it, putting the age out by the UTC offset.
        es = parse(S1A_NAME, S1A_L1, S1A_L2)
        with pytest.raises(ValueError, match="naive"):
            es.age_hours(datetime(2026, 8, 7))

    def test_future_epoch_gives_a_negative_age_rather_than_an_error(self) -> None:
        es = parse(S1A_NAME, S1A_L1, S1A_L2)
        assert es.age_hours(es.epoch - timedelta(hours=5)) == pytest.approx(-5.0)
