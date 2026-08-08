"""Decoding a tasking request into what the sweep actually needs.

Every payload here is a contract example read from disk, VERBATIM. That is not
fussiness: a test that builds its input from the same structure it decodes into
agrees with itself whatever the field names are, which is how #112 shipped a
decoder that produced an all-zero struct without error. A payload the contract
owns is the only input that can tell you whether the mapping is real.

The interesting cases are not the happy path. They are the ones where the
contract permits something the sweep has to reduce: a Polygon target where
`evaluate` wants a point, a `look_side` of ANY where the domain wants None, a
mode the customer asked for that this satellite does not have.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import pytest

from feasibility.messaging.idempotency import NonRetryableError
from feasibility.request import decode
from feasibility.sar import LookSide

ROOT = Path(__file__).resolve().parents[3]
EXAMPLES = ROOT / "contracts" / "examples" / "valid" / "tasking.request.received.v1"


def example(name: str) -> bytes:
    return (EXAMPLES / name).read_bytes()


def patched(name: str, **data: Any) -> bytes:
    """A contract example with specific data fields replaced."""
    envelope = json.loads(example(name))
    envelope["data"].update(data)
    return json.dumps(envelope).encode()


class TestAPointTarget:
    def test_it_decodes_the_fields_the_sweep_consumes(self) -> None:
        request = decode(example("minimal.json"))

        assert request.request_id == "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
        assert request.customer_id == "acme-imaging"
        assert request.window_start == datetime(2026, 8, 7, 10, tzinfo=UTC)
        assert request.window_end == datetime(2026, 8, 8, 10, tzinfo=UTC)
        assert request.requested_modes == ("SCAN",)

    def test_longitude_comes_first_out_of_the_geojson(self) -> None:
        # GeoJSON is [lon, lat] and GroundPoint is (lat, lon). Getting this
        # backwards puts the target in another hemisphere and every subsequent
        # number is computed correctly about the wrong place.
        #
        # The minimal fixture's target is [0, 0], which cannot detect a swap —
        # so this one is patched to a point where the two differ and only one
        # ordering is even in range.
        request = decode(
            patched("minimal.json", target={"type": "Point", "coordinates": [4.4777, 51.9244]})
        )
        assert request.target.longitude_deg == 4.4777
        assert request.target.latitude_deg == 51.9244

    def test_a_point_target_has_no_polygon_to_contain(self) -> None:
        assert decode(example("minimal.json")).target_polygon is None


class TestAPolygonTarget:
    def test_the_access_point_is_the_polygons_centroid(self) -> None:
        # `evaluate` searches access for a POINT. A polygon has to be reduced to
        # one, and the centroid is the defensible choice: it is inside a convex
        # target, and the containment check below is what actually decides
        # whether the whole polygon was covered.
        request = decode(example("polygon-target.json"))

        assert request.target.longitude_deg == pytest.approx(4.10, abs=1e-6)
        assert request.target.latitude_deg == pytest.approx(51.955, abs=1e-6)

    def test_the_polygon_is_kept_so_containment_can_be_checked_against_it(self) -> None:
        # The contract says a Polygon must be fully contained by a SINGLE
        # acquisition footprint — partial coverage across passes is mosaicking
        # and is out of scope. Reducing to a centroid and forgetting the shape
        # would silently turn that into "the centre was covered".
        request = decode(example("polygon-target.json"))

        assert request.target_polygon is not None
        assert request.target_polygon.bounds == pytest.approx((4.02, 51.92, 4.18, 51.99))


class TestConstraints:
    def test_a_customers_narrowing_reaches_the_domain_type(self) -> None:
        constraints = decode(example("polygon-target.json")).constraints

        assert constraints.look_side is LookSide.RIGHT
        assert constraints.min_incidence_deg == 20
        assert constraints.max_incidence_deg == 40
        assert constraints.excluded_satellite_ids == frozenset({"CAPELLA-14"})

    def test_a_request_with_no_constraints_narrows_nothing(self) -> None:
        constraints = decode(example("minimal.json")).constraints

        assert constraints.look_side is None
        assert constraints.min_incidence_deg is None
        assert constraints.excluded_satellite_ids == frozenset()

    def test_look_side_any_becomes_no_opinion_rather_than_a_side(self) -> None:
        # The contract's LookSideConstraint has three values and the domain's
        # LookSide has two. ANY means "either", and mapping it onto a member of
        # a two-valued enum would silently forbid the other side.
        constraints = decode(patched("minimal.json", constraints={"look_side": "ANY"})).constraints
        assert constraints.look_side is None


class TestWhatItRefuses:
    def test_a_payload_that_is_not_json_is_terminal(self) -> None:
        with pytest.raises(NonRetryableError) as caught:
            decode(b"{ not json")
        assert caught.value.reason_code == "INTERNAL_ERROR"

    def test_a_window_that_ends_before_it_starts_is_terminal(self) -> None:
        # JSON Schema cannot express end > start — primitives.v1 says so in as
        # many words — so the check lives in the adapter on both sides of every
        # boundary. This is the feasibility side of it.
        payload = patched(
            "minimal.json",
            window={"start": "2026-08-09T00:00:00Z", "end": "2026-08-08T00:00:00Z"},
        )
        with pytest.raises(NonRetryableError) as caught:
            decode(payload)
        assert caught.value.reason_code == "HORIZON_EXHAUSTED"

    def test_a_geometry_the_sweep_cannot_handle_says_so_by_name(self) -> None:
        # The contract admits Point and Polygon only, and the reason code exists
        # precisely so this is distinguishable from "no access".
        payload = patched(
            "minimal.json",
            target={"type": "LineString", "coordinates": [[4.0, 51.0], [5.0, 52.0]]},
        )
        with pytest.raises(NonRetryableError) as caught:
            decode(payload)
        assert caught.value.reason_code == "UNSUPPORTED_TARGET_GEOMETRY"

    def test_a_target_outside_the_ellipsoid_is_refused_rather_than_propagated(self) -> None:
        # GroundPoint validates its own bounds. Catching it here turns a
        # ValueError deep in the propagation into a named, terminal refusal.
        payload = patched("minimal.json", target={"type": "Point", "coordinates": [400.0, 51.0]})
        with pytest.raises(NonRetryableError) as caught:
            decode(payload)
        assert caught.value.reason_code == "UNSUPPORTED_TARGET_GEOMETRY"
