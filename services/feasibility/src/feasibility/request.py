"""Turning a tasking request on the wire into what the sweep needs.

The boundary, and it does three reductions the contract permits and `evaluate`
does not:

  A POLYGON BECOMES A POINT PLUS A SHAPE. `evaluate` searches access for one
  ground point. A polygon target is reduced to its centroid for that search and
  KEPT for the containment check, because the contract is explicit that a
  Polygon must be fully contained by a single acquisition footprint — partial
  coverage across passes is mosaicking and is out of scope. Reducing to a
  centroid and forgetting the shape would quietly turn that guarantee into "the
  middle was covered".

  LOOK SIDE `ANY` BECOMES None. The contract's LookSideConstraint has three
  values and the domain's LookSide has two, because ANY is the absence of a
  constraint rather than a third side. Mapping it onto a member would silently
  forbid the other side.

  A MALFORMED REQUEST BECOMES A NAMED REFUSAL. Every failure here raises
  NonRetryableError with a reason_code the contract publishes, so the customer
  is told which of their inputs the system could not use — rather than getting
  NO_ACCESS_IN_HORIZON, which would be a lie about the physics.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import datetime
from typing import TYPE_CHECKING, Any

from shapely.geometry import Polygon, shape

from feasibility.messaging.idempotency import NonRetryableError
from feasibility.orbit import GroundPoint
from feasibility.sar import AcquisitionConstraints, LookSide

if TYPE_CHECKING:
    pass

# What the contract's TargetGeometry admits. Anything else is a request this
# system declined to support, and says so by name.
_SUPPORTED_GEOMETRIES = ("Point", "Polygon")


@dataclass(frozen=True)
class SweepRequest:
    """One customer request, in the terms the sweep works in."""

    request_id: str
    customer_id: str

    # Where the access search points. For a Polygon this is the centroid.
    target: GroundPoint

    # The shape a footprint must contain, or None for a point target. Kept
    # separate from `target` so that neither can be mistaken for the other.
    target_polygon: Polygon | None

    window_start: datetime
    window_end: datetime
    requested_modes: tuple[str, ...]
    constraints: AcquisitionConstraints


def decode(payload: bytes) -> SweepRequest:
    """Decode `tasking.request.received.v1`, or refuse by name."""
    try:
        envelope = json.loads(payload)
        data = envelope["data"]
    except (json.JSONDecodeError, KeyError, TypeError) as exc:
        raise NonRetryableError(
            "INTERNAL_ERROR", "the request payload has no readable data object"
        ) from exc

    target, polygon = _target_of(data)
    start, end = _window_of(data)

    return SweepRequest(
        request_id=str(data["request_id"]),
        customer_id=str(data["customer_id"]),
        target=target,
        target_polygon=polygon,
        window_start=start,
        window_end=end,
        requested_modes=tuple(data.get("requested_modes", ())),
        constraints=_constraints_of(data.get("constraints") or {}),
    )


def _target_of(data: dict[str, Any]) -> tuple[GroundPoint, Polygon | None]:
    geometry = data.get("target") or {}
    kind = geometry.get("type")
    if kind not in _SUPPORTED_GEOMETRIES:
        raise NonRetryableError(
            "UNSUPPORTED_TARGET_GEOMETRY",
            f"target geometry is {kind!r}; this system accepts "
            f"{' and '.join(_SUPPORTED_GEOMETRIES)} only",
        )

    try:
        if kind == "Point":
            longitude, latitude = geometry["coordinates"][:2]
            return GroundPoint(latitude_deg=float(latitude), longitude_deg=float(longitude)), None

        polygon = shape(geometry)
        centroid = polygon.centroid
        # Longitude first out of GeoJSON, and `centroid.x` is that longitude.
        # Stated because the swap is silent: every downstream number would be
        # computed correctly, about a place on the other side of the world.
        point = GroundPoint(latitude_deg=float(centroid.y), longitude_deg=float(centroid.x))
    except (KeyError, IndexError, TypeError, ValueError) as exc:
        # GroundPoint validates its own bounds, so a longitude of 400 arrives
        # here as a ValueError. Naming it turns a crash deep in the propagation
        # into an answer the customer can act on.
        raise NonRetryableError(
            "UNSUPPORTED_TARGET_GEOMETRY", f"target geometry could not be read: {exc}"
        ) from exc

    return point, polygon


def _window_of(data: dict[str, Any]) -> tuple[datetime, datetime]:
    window = data.get("window") or {}
    try:
        start = _timestamp(window["start"])
        end = _timestamp(window["end"])
    except (KeyError, TypeError, ValueError) as exc:
        raise NonRetryableError("INTERNAL_ERROR", f"request window is unreadable: {exc}") from exc

    if end <= start:
        # JSON Schema cannot express end > start — primitives.v1 says so in as
        # many words — so the check lives in the adapter on both sides of every
        # boundary. This is the feasibility side.
        raise NonRetryableError(
            "HORIZON_EXHAUSTED",
            f"the request window ends at or before it starts: {start.isoformat()} "
            f"to {end.isoformat()}",
        )
    return start, end


def _constraints_of(raw: dict[str, Any]) -> AcquisitionConstraints:
    side = raw.get("look_side")
    return AcquisitionConstraints(
        # ANY is the absence of a constraint, not a third side.
        look_side=LookSide(side) if side in ("LEFT", "RIGHT") else None,
        min_incidence_deg=_optional_float(raw.get("min_incidence_deg")),
        max_incidence_deg=_optional_float(raw.get("max_incidence_deg")),
        max_squint_deg=_optional_float(raw.get("max_squint_deg")),
        excluded_satellite_ids=frozenset(raw.get("excluded_satellite_ids") or ()),
    )


def _optional_float(value: Any) -> float | None:
    return None if value is None else float(value)


def _timestamp(value: str) -> datetime:
    """RFC 3339 with a trailing Z, which `fromisoformat` does not accept alone.

    Python 3.11 onwards parses `Z`, but the replacement is kept explicit: the
    contract states UTC with a trailing Z, and relying on an interpreter version
    to be lenient about it is a dependency nobody wrote down.
    """
    return datetime.fromisoformat(value.replace("Z", "+00:00"))
