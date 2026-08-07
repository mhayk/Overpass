"""SAR acquisition geometry, sensor limits, and geodesic footprints.

SAR is side-looking. A target directly beneath the satellite is not imageable —
see geometry.py, and the nadir test that guards it.
"""

from feasibility.sar.footprint import area_km2, contains_target, ground_footprint
from feasibility.sar.geometry import (
    AccessGeometry,
    ImagingMode,
    LookSide,
    compute,
)
from feasibility.sar.sensor import (
    AcquisitionConstraints,
    SensorMode,
    effective_limits,
    quality_score,
    satisfies,
)

__all__ = [
    "AccessGeometry",
    "AcquisitionConstraints",
    "ImagingMode",
    "LookSide",
    "SensorMode",
    "area_km2",
    "compute",
    "contains_target",
    "effective_limits",
    "ground_footprint",
    "quality_score",
    "satisfies",
]
