"""Tests for the Celestrak adapter, against a mocked transport.

No test here touches the network. ADR-0011 splits the two uses of TLE data
deliberately: live fetch at seed time, frozen snapshot for anything that must be
reproducible. A unit test that reached Celestrak would be neither — it would
fail on a plane and pass differently every day.
"""

from __future__ import annotations

import httpx
import pytest
import respx

from feasibility.tle import CelestrakClient, CelestrakError

GP_URL = "https://celestrak.org/NORAD/elements/gp.php"

BODY = (
    "SENTINEL-1A             \r\n"
    "1 39634U 14016A   26217.94446112  .00000085  00000+0  27560-4 0  9992\r\n"
    "2 39634  98.1585 224.7210 0001387  84.4412 275.6946 14.59278904657278\r\n"
)


@respx.mock
def test_fetch_by_catalog_number() -> None:
    respx.get(GP_URL).mock(return_value=httpx.Response(200, text=BODY))
    sets = CelestrakClient().fetch_by_catalog_number(39634)
    assert len(sets) == 1
    assert sets[0].norad_id == 39634
    assert sets[0].name == "SENTINEL-1A"


@respx.mock
def test_trailing_whitespace_and_crlf_survive_the_round_trip() -> None:
    # Celestrak pads names to a fixed width and uses CRLF. A parser that did not
    # strip either would produce names with trailing spaces and lines 70
    # characters long, which fails the length check for the wrong reason.
    respx.get(GP_URL).mock(return_value=httpx.Response(200, text=BODY))
    (es,) = CelestrakClient().fetch_by_name("SENTINEL-1A")
    assert es.name == "SENTINEL-1A"
    assert len(es.line1) == 69


@respx.mock
def test_no_gp_data_is_an_error_not_an_empty_list() -> None:
    # Celestrak answers an unmatched query with 200 and this body, not a 404.
    # Treating it as an empty catalogue would seed a constellation with no
    # satellites and the first symptom would appear three services downstream.
    respx.get(GP_URL).mock(return_value=httpx.Response(200, text="No GP data found"))
    with pytest.raises(CelestrakError, match="no element sets"):
        CelestrakClient().fetch_by_catalog_number(1)


@respx.mock
def test_empty_body_is_an_error() -> None:
    respx.get(GP_URL).mock(return_value=httpx.Response(200, text="   \n"))
    with pytest.raises(CelestrakError, match="no element sets"):
        CelestrakClient().fetch_by_catalog_number(1)


@respx.mock
def test_http_error_is_wrapped() -> None:
    respx.get(GP_URL).mock(return_value=httpx.Response(503))
    with pytest.raises(CelestrakError, match="failed"):
        CelestrakClient().fetch_by_catalog_number(39634)


@respx.mock
def test_timeout_is_wrapped() -> None:
    respx.get(GP_URL).mock(side_effect=httpx.ConnectTimeout("too slow"))
    with pytest.raises(CelestrakError, match="failed"):
        CelestrakClient().fetch_by_catalog_number(39634)


@respx.mock
def test_garbage_body_is_wrapped_rather_than_raised_raw() -> None:
    respx.get(GP_URL).mock(return_value=httpx.Response(200, text="not a tle at all\n"))
    with pytest.raises(CelestrakError, match="unparseable"):
        CelestrakClient().fetch_by_catalog_number(39634)


@respx.mock
def test_fetch_many_issues_one_request_per_object() -> None:
    route = respx.get(GP_URL).mock(return_value=httpx.Response(200, text=BODY))
    CelestrakClient().fetch_many([39634, 41456, 62261])
    assert route.call_count == 3
