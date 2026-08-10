"""Celestrak adapter: the only place in the service that knows about the network.

Kept deliberately thin. It fetches text and hands it to `element_set.parse_catalogue`;
it does not decide what a valid TLE is, because then there would be two answers
to that question and they would drift.

ADR-0011 governs when this is used at all: live fetch at seed time, frozen
snapshot from `testdata/tle/` for anything that has to be reproducible. Golden
reference tests never touch this class.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import TYPE_CHECKING

import httpx

from feasibility import metrics
from feasibility.resilience import Breaker, BreakerOpenError
from feasibility.tle.element_set import ElementSet, TleFormatError, parse_catalogue

if TYPE_CHECKING:
    from collections.abc import Sequence

_DEFAULT_BASE_URL = "https://celestrak.org/NORAD/elements/gp.php"


class CelestrakError(RuntimeError):
    """Celestrak could not be reached, or returned something unusable."""


@dataclass(frozen=True)
class CelestrakClient:
    """Fetches element sets from Celestrak's GP query endpoint.

    `timeout_s` is short on purpose. This runs at startup, and a hung fetch
    against an unreachable upstream should fail fast into the frozen snapshot
    rather than hold the service in a pending state — a system that will not
    start is harder to diagnose than one that starts and says its data is old.
    """

    base_url: str = _DEFAULT_BASE_URL
    timeout_s: float = 10.0
    user_agent: str = "overpass-feasibility/0.1 (+https://github.com/mhayk/Overpass)"

    breaker: Breaker = field(default_factory=Breaker)
    """Trips after three consecutive failures (#51).

    `fetch_many` makes one request per satellite, so an upstream that hangs
    costs `timeout_s` PER SATELLITE — ten seconds each across a constellation of
    tens is minutes of a service that looks stuck at startup. The breaker turns
    everything after the third failure into an immediate refusal, and the caller
    reaches its frozen snapshot promptly instead of eventually.

    Shared across calls by construction: it is the client that has an opinion
    about the upstream, not any single fetch.
    """

    def fetch_by_name(self, name: str) -> list[ElementSet]:
        """Fetch every catalogued object whose name contains `name`."""
        return self._fetch({"NAME": name, "FORMAT": "tle"})

    def fetch_by_catalog_number(self, norad_id: int) -> list[ElementSet]:
        """Fetch one object by NORAD catalog number."""
        return self._fetch({"CATNR": str(norad_id), "FORMAT": "tle"})

    def fetch_many(self, norad_ids: Sequence[int]) -> list[ElementSet]:
        """Fetch several objects, one request each.

        Celestrak's GP endpoint takes a single CATNR, so this is N requests by
        necessity rather than by choice. It is called once at seed time for a
        constellation of tens, not per acquisition — if that ever changes, the
        fix is a GROUP query and not a thread pool.
        """
        out: list[ElementSet] = []
        for norad_id in norad_ids:
            out.extend(self.fetch_by_catalog_number(norad_id))
        return out

    def __post_init__(self) -> None:
        """Publish this breaker's state as a gauge (#51).

        Registered at CONSTRUCTION rather than from a composition root,
        because the composition root that would do it does not exist: nothing
        in the running services builds a CelestrakClient. ADR-0011 makes the
        seeder read the frozen snapshot in testdata rather than the network,
        deliberately, so the only outbound HTTP this system has is currently
        unreachable from any running process.

        That is stated here rather than worked around. The M3-04 spec already
        made this argument one level up — "a breaker with no caller is dead
        code that looks like resilience" — and the same applies to its metric:
        a gauge wired from a composition root nobody runs would read a
        constant zero and imply a healthy dependency that is simply never
        called. Registering here means the series appears the moment live TLE
        refresh gives the client a caller, and is honestly absent until then.
        """
        metrics.instruments().register_breaker(
            "celestrak", lambda: int(self.breaker.state.value)
        )

    def _request(self, params: dict[str, str]) -> httpx.Response:
        """One HTTP call, with every transport failure shaped as a CelestrakError.

        Split out so the breaker wraps the NETWORK call and nothing else. An
        empty-but-valid answer is a data problem, not an upstream fault, and
        counting it as one would trip the breaker on a query that will never
        match however healthy Celestrak is.
        """
        try:
            response = httpx.get(
                self.base_url,
                params=params,
                timeout=self.timeout_s,
                headers={"User-Agent": self.user_agent},
                follow_redirects=True,
            )
            response.raise_for_status()
        except httpx.HTTPError as exc:
            msg = f"fetching {params} from Celestrak failed: {exc}"
            raise CelestrakError(msg) from exc
        return response

    def _fetch(self, params: dict[str, str]) -> list[ElementSet]:
        try:
            response = self.breaker.call(lambda: self._request(params))
        except BreakerOpenError as refused:
            # Reported as a CelestrakError so every caller's fallback path is
            # unchanged, with the reason preserved: "gave up on it" and "it
            # failed again" look identical in a log otherwise, and they call for
            # different actions.
            msg = f"skipping Celestrak for {params}: {refused}"
            raise CelestrakError(msg) from refused

        body = response.text.strip()

        # Celestrak answers an unmatched query with 200 and a body saying "No GP
        # data found" — not a 404. Treating that as an empty catalogue would
        # silently seed a constellation with no satellites in it, and the first
        # symptom would be a feasibility sweep that finds no opportunities for
        # reasons nobody can trace back to here.
        if not body or "No GP data found" in body:
            msg = f"Celestrak returned no element sets for {params}"
            raise CelestrakError(msg)

        try:
            return parse_catalogue(body)
        except TleFormatError as exc:
            msg = f"Celestrak returned an unparseable catalogue for {params}: {exc}"
            raise CelestrakError(msg) from exc
