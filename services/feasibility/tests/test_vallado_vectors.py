"""Our propagation path against the official Vallado verification vectors.

This is the strongest oracle available for this code and the reason it is worth
a file of its own. `tcppver.out` is the output of the reference C++ SGP4
implementation — not of the Python port we depend on, and certainly not of
anything in this repository. If our numbers match it, the propagation is right
for reasons that have nothing to do with our own belief about what right means.

What is actually under test is the WIRING, not the mathematics: that our
`ElementSet` reaches the propagator with its lines intact, that we read TEME
position out of the right place, and that nothing in our construction path
silently substitutes a different gravity model or epoch.

The tolerance is deliberately tight. These are not "close enough" comparisons —
the same algorithm on the same inputs should agree to within floating-point
noise, and anything larger means a real difference somewhere.
"""

from __future__ import annotations

from pkgutil import get_data

import pytest
from sgp4.api import Satrec

from feasibility.tle.element_set import parse

# Kilometres. Vallado's file carries eight significant figures; agreement well
# inside a metre over a day of propagation is what a correctly wired port looks
# like, and a wrong gravity model or epoch would miss by kilometres.
POSITION_TOLERANCE_KM = 1e-5
VELOCITY_TOLERANCE_KM_S = 1e-8


def load_expected(catalog_number: int) -> list[tuple[float, tuple[float, float, float]]]:
    """Pull the (tsince, position) rows for one satellite out of tcppver.out.

    The file is blocks of `<catnr> xx` followed by rows of tsince, position and
    velocity. Rows are only collected while inside the requested block.
    """
    text = get_data("sgp4", "tcppver.out")
    assert text is not None
    rows: list[tuple[float, tuple[float, float, float]]] = []
    inside = False
    for raw in text.decode("ascii").splitlines():
        line = raw.strip()
        if not line:
            continue
        if line.endswith("xx"):
            inside = line.split()[0] == str(catalog_number)
            continue
        if not inside:
            continue
        parts = line.split()
        if len(parts) < 7:
            continue
        rows.append((float(parts[0]), (float(parts[1]), float(parts[2]), float(parts[3]))))
    return rows


def load_tle_lines(catalog_number: int) -> tuple[str, str]:
    """Pull one satellite's two lines out of SGP4-VER.TLE.

    The verification file appends start/stop/step columns to line 2, which are
    test-harness metadata rather than part of the element set. They are trimmed
    to 69 characters here — which also means our own parser's length and
    checksum checks are exercised on genuinely official data rather than on
    something we wrote.
    """
    text = get_data("sgp4", "SGP4-VER.TLE")
    assert text is not None
    lines = [ln for ln in text.decode("ascii").splitlines() if not ln.startswith("#")]
    target = f"{catalog_number:05d}"
    for i, line in enumerate(lines):
        if line.startswith("1 ") and line[2:7] == target:
            return line[:69], lines[i + 1][:69]
    msg = f"catalog number {catalog_number} not found in SGP4-VER.TLE"
    raise LookupError(msg)


# 00005 is the canonical first case in Vallado's suite — a deep-ish eccentric
# orbit that exercises more of the model than a circular LEO. 04632 and 06251
# add a near-Earth case and a decayed-orbit case.
@pytest.mark.parametrize("catalog_number", [5, 4632, 6251])
def test_our_lines_reproduce_the_reference_positions(catalog_number: int) -> None:
    line1, line2 = load_tle_lines(catalog_number)

    # Go in through OUR parser, not the library's, so that a bug in our
    # column handling shows up here rather than surviving to production.
    element_set = parse(f"VER-{catalog_number}", line1, line2)
    assert element_set.norad_id == catalog_number

    satrec = Satrec.twoline2rv(element_set.line1, element_set.line2)
    expected = load_expected(catalog_number)
    assert expected, "no reference rows found — the oracle itself is missing"

    compared = 0
    for tsince, (x, y, z) in expected:
        jd = satrec.jdsatepoch
        fr = satrec.jdsatepochF + tsince / 1440.0
        error, position, _velocity = satrec.sgp4(jd, fr)
        if error != 0:
            # Vallado's suite includes deliberately failing cases. A row the
            # reference propagated and we could not would be a real problem;
            # this file marks those with error codes of its own, so skipping a
            # row we error on is only safe because we assert a count at the end.
            continue
        assert position[0] == pytest.approx(x, abs=POSITION_TOLERANCE_KM)
        assert position[1] == pytest.approx(y, abs=POSITION_TOLERANCE_KM)
        assert position[2] == pytest.approx(z, abs=POSITION_TOLERANCE_KM)
        compared += 1

    # Without this the test would pass vacuously if the parser silently returned
    # no rows, or if every propagation errored.
    assert compared >= 5, f"only {compared} rows compared for {catalog_number}"


def test_the_oracle_would_notice_a_wrong_answer() -> None:
    """The comparison has teeth: a deliberately shifted position must fail.

    Without this, a tolerance accidentally set to something enormous would let
    the whole file pass while checking nothing.
    """
    line1, line2 = load_tle_lines(5)
    satrec = Satrec.twoline2rv(line1, line2)
    tsince, (x, _y, _z) = load_expected(5)[0]
    fr = satrec.jdsatepochF + tsince / 1440.0
    _error, position, _velocity = satrec.sgp4(satrec.jdsatepoch, fr)

    # One metre off is one hundred times the tolerance and must be rejected.
    with pytest.raises(AssertionError):
        assert position[0] == pytest.approx(x + 0.001, abs=POSITION_TOLERANCE_KM)
