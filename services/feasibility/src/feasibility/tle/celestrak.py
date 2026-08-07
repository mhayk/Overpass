"""Celestrak adapter: the only place in the service that knows about the network.

Kept deliberately thin. It fetches text and hands it to `element_set.parse_catalogue`;
it does not decide what a valid TLE is, because then there would be two answers
to that question and they would drift.

ADR-0011 governs when this is used at all: live fetch at seed time, frozen
snapshot from `testdata/tle/` for anything that has to be reproducible. Golden
reference tests never touch this class.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import TYPE_CHECKING

import httpx

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

    def _fetch(self, params: dict[str, str]) -> list[ElementSet]:
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
