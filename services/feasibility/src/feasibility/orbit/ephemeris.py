"""Sampling a satellite's position over a bucket of time.

No orbital mechanics here either — every position comes from `Propagator`, which
holds Skyfield, which holds the reference SGP4. This module decides only WHEN to
ask and what shape the answer travels in.

Two decisions live here and both are recorded in
`docs/decisions/0016-ephemeris-sampling-and-horizon.md`:

  THE INTERVAL IS TEN SECONDS. A LEO SAR satellite covers about 66 km of ground
  track in ten seconds. Cesium interpolates between samples, and a coarser step
  visibly cuts the corner where the track curves hardest — near the poles, where
  a sun-synchronous constellation spends most of its time.

  THE BUCKETS ARE ALIGNED TO A FIXED GRID, not to whenever the sweep ran. The
  published event id is derived from the bucket start, so an unaligned grid would
  make every sweep publish a fresh, overlapping track instead of colliding
  harmlessly with the one already in the outbox.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from feasibility.orbit.propagation import Propagator

# One sample every ten seconds, three-hour buckets, a day of horizon.
#
# The bucket matches the planner's allocation bucket so that rendering one plan
# needs one ephemeris row range rather than a stitched-together set — see the
# gateway's reads. The horizon matches the demo's request windows: publishing
# further ahead is free to compute and is payload nobody looks at.
_DEFAULT_INTERVAL_S = 10.0
_DEFAULT_BUCKET_S = 3 * 3600.0
_DEFAULT_HORIZON_S = 24 * 3600.0

# The grid's origin. The Unix epoch rather than "midnight today", because
# "today" depends on when you ask and the whole point of the grid is that it
# does not.
_GRID_ORIGIN = datetime(1970, 1, 1, tzinfo=UTC)


@dataclass(frozen=True)
class SamplingPolicy:
    """How finely, how far ahead, and in what size of bucket."""

    interval_s: float = _DEFAULT_INTERVAL_S
    bucket_s: float = _DEFAULT_BUCKET_S
    horizon_s: float = _DEFAULT_HORIZON_S

    def __post_init__(self) -> None:
        if self.interval_s <= 0:
            msg = f"interval must be positive, got {self.interval_s}"
            raise ValueError(msg)
        if self.bucket_s < 2 * self.interval_s:
            msg = (
                "a bucket must hold at least two samples, so bucket_s must be at least "
                f"twice interval_s; got {self.bucket_s} and {self.interval_s}"
            )
            raise ValueError(msg)
        if self.horizon_s < self.bucket_s:
            msg = f"horizon {self.horizon_s} is shorter than one bucket {self.bucket_s}"
            raise ValueError(msg)


@dataclass(frozen=True)
class EphemerisTrack:
    """One satellite's sampled position over one bucket.

    `samples` are `(offset_s, longitude_deg, latitude_deg, altitude_m)` tuples,
    in the order the contract states. Longitude first, as everywhere else in
    this system — the swap is silent and puts a satellite in the wrong
    hemisphere while every number stays inside its bounds.
    """

    satellite_id: str
    epoch: datetime
    horizon_start: datetime
    horizon_end: datetime
    interval_s: float
    samples: list[tuple[float, float, float, float]]


def bucket_starts(now: datetime, policy: SamplingPolicy | None = None) -> list[datetime]:
    """The aligned bucket starts covering `[now, now + horizon)`.

    The first bucket is the one CONTAINING `now`, not the next one to begin.
    Starting at the next boundary would leave the globe with no track for the
    current instant, which is the one instant a viewer is looking at.

    `now` is passed rather than read from the clock, for the same reason
    `evaluate` takes it: a sweep that consults wall time is not reproducible.
    """
    policy = policy or SamplingPolicy()
    if now.tzinfo is None:
        msg = "refusing to bucket a naive datetime"
        raise ValueError(msg)

    bucket = timedelta(seconds=policy.bucket_s)
    horizon_end = now + timedelta(seconds=policy.horizon_s)

    first = _floor_to_grid(now, policy.bucket_s)
    out: list[datetime] = []
    cursor = first
    while cursor < horizon_end:
        out.append(cursor)
        cursor += bucket
    return out


def _floor_to_grid(when: datetime, bucket_s: float) -> datetime:
    elapsed = (when.astimezone(UTC) - _GRID_ORIGIN).total_seconds()
    return _GRID_ORIGIN + timedelta(seconds=math.floor(elapsed / bucket_s) * bucket_s)


def sample_track(
    satellite_id: str,
    propagator: Propagator,
    start: datetime,
    end: datetime,
    interval_s: float,
) -> EphemerisTrack:
    """Propagate `[start, end)` at `interval_s` and return the sampled track.

    Half-open, like every other interval in this system. The sample at exactly
    `end` belongs to the next bucket; emitting it in both would put two
    positions at one instant and leave the projection's primary key to decide
    which one survives.

    `satellite_id` is passed in rather than taken from the element set's name.
    They are not the same string — the seeder derives the id from the Celestrak
    name by stripping the parenthetical, so `CAPELLA-11 (ACADIA-1)` is the name
    and `CAPELLA-11` is the id — and using the name would produce an event whose
    satellite_id matches nothing in `reference.satellites`.
    """
    if interval_s <= 0:
        msg = f"interval must be positive, got {interval_s}"
        raise ValueError(msg)

    duration_s = (end - start).total_seconds()
    count = math.ceil(duration_s / interval_s)
    if count < 2:
        msg = (
            f"a track needs at least two samples; {duration_s} s at {interval_s} s "
            "yields fewer. One point is a position, not a path."
        )
        raise ValueError(msg)

    offsets = [interval_s * i for i in range(count)]
    instants = [start + timedelta(seconds=offset) for offset in offsets]
    subpoints = propagator.subpoints(instants)

    return EphemerisTrack(
        satellite_id=satellite_id,
        epoch=start,
        horizon_start=start,
        horizon_end=end,
        interval_s=interval_s,
        samples=[
            (offset, point.longitude_deg, point.latitude_deg, point.elevation_m)
            for offset, point in zip(offsets, subpoints, strict=True)
        ],
    )
