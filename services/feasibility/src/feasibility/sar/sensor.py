"""Sensor limits, customer constraints, and the filter that applies both.

Two layers of narrowing, and the order is not interchangeable:

  The SENSOR says what the instrument can physically do. Those limits are
  configuration — the contract states the incidence band as "configuration, not
  contract (default 15-45 degrees)" so that a differently-specified sensor does
  not require a schema version.

  The CUSTOMER says what they will accept. Constraints can only ever NARROW,
  never widen: a customer cannot request an incidence angle the sensor does not
  support, and a request that tried would be asking for an image physics will
  not produce.

`effective_limits` enforces that direction explicitly rather than trusting
callers to intersect correctly.
"""

from __future__ import annotations

from dataclasses import dataclass, field, replace

from feasibility.sar.geometry import AccessGeometry, ImagingMode, LookSide

# Defaults from sar.v1.schema.json's description text. Stated once, here, so
# that a change is one edit rather than a hunt.
_DEFAULT_MIN_INCIDENCE_DEG = 15.0
_DEFAULT_MAX_INCIDENCE_DEG = 45.0


@dataclass(frozen=True)
class SensorMode:
    """What one imaging mode of one instrument can physically do.

    Mirrors sar.v1.schema.json#/$defs/SensorModeParameters. Stored per satellite
    in reference.satellites.sensor_modes, which is one of the four JSONB cases
    ADR-0004 permits precisely because these fields differ by mode and keep
    changing as the geometry model is refined.
    """

    mode: ImagingMode
    swath_width_km: float
    resolution_m: float
    min_dwell_s: float
    max_dwell_s: float
    min_incidence_deg: float = _DEFAULT_MIN_INCIDENCE_DEG
    max_incidence_deg: float = _DEFAULT_MAX_INCIDENCE_DEG
    max_squint_deg: float = 5.0
    max_slant_range_km: float = 2500.0
    permitted_look_sides: tuple[LookSide, ...] = (LookSide.LEFT, LookSide.RIGHT)

    def __post_init__(self) -> None:
        if not 0.0 <= self.min_incidence_deg < self.max_incidence_deg <= 90.0:
            msg = (
                "incidence band must satisfy 0 <= min < max <= 90, got "
                f"{self.min_incidence_deg}..{self.max_incidence_deg}"
            )
            raise ValueError(msg)
        if not 0.0 <= self.max_squint_deg <= 90.0:
            msg = f"max_squint_deg out of range: {self.max_squint_deg}"
            raise ValueError(msg)
        if self.swath_width_km <= 0 or self.max_slant_range_km <= 0:
            msg = "swath width and slant range bound must be positive"
            raise ValueError(msg)
        if not 0 < self.min_dwell_s <= self.max_dwell_s:
            msg = (
                "dwell band must satisfy 0 < min <= max, got "
                f"{self.min_dwell_s}..{self.max_dwell_s}"
            )
            raise ValueError(msg)
        if not self.permitted_look_sides:
            msg = "a mode with no permitted look side can never image anything"
            raise ValueError(msg)


@dataclass(frozen=True)
class AcquisitionConstraints:
    """Customer-supplied narrowing. Every field optional; None means no opinion.

    Mirrors sar.v1.schema.json#/$defs/AcquisitionConstraints.
    """

    look_side: LookSide | None = None
    min_incidence_deg: float | None = None
    max_incidence_deg: float | None = None
    max_squint_deg: float | None = None
    excluded_satellite_ids: frozenset[str] = field(default_factory=frozenset)


def effective_limits(mode: SensorMode, constraints: AcquisitionConstraints) -> SensorMode | None:
    """Intersect customer constraints with sensor limits.

    Returns None when the narrowing leaves nothing achievable — an incidence
    band that has collapsed, or every look side excluded. That is a request
    which cannot be satisfied, not an error, and None says so without
    constructing a SensorMode that violates its own invariants.

    Narrowing only, in every direction. A customer asking for incidence down to
    5 degrees on a sensor whose floor is 15 gets 15, not 5 — the alternative is
    promising an image the instrument cannot take, and discovering that at
    acquisition time rather than at submission time.
    """
    min_incidence = mode.min_incidence_deg
    if constraints.min_incidence_deg is not None:
        min_incidence = max(min_incidence, constraints.min_incidence_deg)

    max_incidence = mode.max_incidence_deg
    if constraints.max_incidence_deg is not None:
        max_incidence = min(max_incidence, constraints.max_incidence_deg)

    max_squint = mode.max_squint_deg
    if constraints.max_squint_deg is not None:
        max_squint = min(max_squint, constraints.max_squint_deg)

    sides = mode.permitted_look_sides
    if constraints.look_side is not None:
        sides = tuple(s for s in sides if s is constraints.look_side)

    if min_incidence >= max_incidence or not sides:
        return None

    return replace(
        mode,
        min_incidence_deg=min_incidence,
        max_incidence_deg=max_incidence,
        max_squint_deg=max_squint,
        permitted_look_sides=sides,
    )


def satisfies(geometry: AccessGeometry, limits: SensorMode | None) -> bool:
    """Whether this instant of geometry is imageable under these limits.

    A None limit set means the customer's constraints could not be reconciled
    with the sensor at all, so nothing satisfies it.

    All four conditions simultaneously, per the contract: incidence inside the
    band, look side permitted, squint within the steering limit, slant range in
    bounds. Any one of them failing makes the acquisition impossible, so this is
    an AND and not a score.
    """
    if limits is None:
        return False
    if not limits.min_incidence_deg <= geometry.incidence_angle_deg <= limits.max_incidence_deg:
        return False
    if geometry.look_side not in limits.permitted_look_sides:
        return False
    if abs(geometry.squint_angle_deg) > limits.max_squint_deg:
        return False
    return geometry.slant_range_km <= limits.max_slant_range_km


def quality_score(geometry: AccessGeometry, limits: SensorMode | None) -> float:
    """Normalised geometric quality, 0 to 1.

    NOT A VALUE. Value comes from the bid and the priority tier, and conflating
    the two would quietly corrupt the allocation mechanism — a geometrically
    excellent acquisition for a customer who bid nothing would start outranking
    a merely adequate one for a customer who bid a lot. This is a tie-break
    input only, and keeping it separate is what keeps the auction honest.

    Two terms, multiplied so that either can veto:

      Incidence position. Best at the centre of the band, falling to zero at
      the edges. Mid-band is where resolution and backscatter trade off most
      favourably; the extremes are usable but worse, which is the whole reason
      the band has a middle.

      Squint magnitude. Best at broadside, falling linearly to zero at the
      steering limit. A heavily squinted acquisition is achievable and degraded.
    """
    if limits is None or not satisfies(geometry, limits):
        return 0.0

    centre = (limits.min_incidence_deg + limits.max_incidence_deg) / 2.0
    half_width = (limits.max_incidence_deg - limits.min_incidence_deg) / 2.0
    incidence_term = 1.0 - abs(geometry.incidence_angle_deg - centre) / half_width

    if limits.max_squint_deg == 0.0:
        squint_term = 1.0
    else:
        squint_term = 1.0 - abs(geometry.squint_angle_deg) / limits.max_squint_deg

    return max(0.0, min(1.0, incidence_term * squint_term))
