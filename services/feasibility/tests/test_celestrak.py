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

from feasibility.resilience import Breaker, State
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


# --- the breaker (#51) -------------------------------------------------------


@respx.mock
def test_a_dependency_that_keeps_failing_stops_being_called() -> None:
    """The claim #51 makes: the caller degrades rather than queueing.

    `fetch_many` is one request per satellite, so an upstream that hangs costs
    the full timeout PER SATELLITE. After the third consecutive failure the
    breaker refuses outright, and the caller reaches its frozen snapshot in
    milliseconds instead of minutes.
    """
    route = respx.get(url__startswith=CelestrakClient.base_url).mock(
        return_value=httpx.Response(503)
    )
    client = CelestrakClient(breaker=Breaker(threshold=3, cooldown_s=60))

    for _ in range(3):
        with pytest.raises(CelestrakError):
            client.fetch_by_catalog_number(25544)
    assert route.call_count == 3

    with pytest.raises(CelestrakError, match="skipping Celestrak"):
        client.fetch_by_catalog_number(25544)

    # The number that matters: the fourth call never reached the network.
    assert route.call_count == 3, "an open breaker still called the upstream it had given up on"
    assert client.breaker.state is State.OPEN


@respx.mock
def test_an_empty_answer_is_a_data_problem_and_does_not_trip_the_breaker() -> None:
    """Celestrak answers an unmatched query with 200 and "No GP data found".

    That is a query nobody will ever match, not an upstream fault. Counting it
    as one would open the breaker against a perfectly healthy dependency and
    take the real satellites down with it.
    """
    respx.get(url__startswith=CelestrakClient.base_url).mock(
        return_value=httpx.Response(200, text="No GP data found")
    )
    client = CelestrakClient(breaker=Breaker(threshold=2, cooldown_s=60))

    for _ in range(3):
        with pytest.raises(CelestrakError):
            client.fetch_by_catalog_number(99999)

    assert client.breaker.state is State.CLOSED
