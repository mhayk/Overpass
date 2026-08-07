"""Coarse-to-fine access-window search.

The problem: find every interval in a horizon during which a target is visible
above an elevation mask, for one satellite, cheaply enough to do it for a whole
constellation over days.

The naive approach — step finely enough never to miss a short window — is far
too slow. A LEO pass lasts minutes, so "never miss" means a step of seconds,
which across ten satellites and a 72-hour horizon is millions of propagations.

So: coarse sample to find sign changes in (elevation - mask), then bisect each
bracket to a tolerance. The correctness condition is stated and enforced rather
than assumed — see `AccessSearchPolicy.coarse_step_s`.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timedelta
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from collections.abc import Sequence

    from feasibility.orbit.propagation import GroundPoint, Propagator


@dataclass(frozen=True)
class AccessSearchPolicy:
    """How the search trades cost against the risk of missing a window.

    `elevation_mask_deg` is deliberately PERMISSIVE. It is not the SAR
    constraint — that is incidence angle, applied in M1-11, and its default band
    of 15 to 45 degrees of incidence corresponds to 45 to 75 degrees of
    elevation. A coarse pass at 5 degrees therefore cannot lose a window the SAR
    filter would have kept, and a coarse pass tighter than the downstream filter
    would silently narrow the candidate set. Permissive here, strict there.

    `coarse_step_s` MUST be shorter than the shortest window that can exist, or
    a whole pass can fall between two samples and never be found — the failure
    mode that produces no error, no log line, and a request that looks simply
    infeasible. At 5 degrees of elevation a LEO pass lasts several minutes, so
    60 seconds carries a comfortable margin. Raising the mask shortens passes;
    raising it far enough would make this step unsafe, which is why
    `__post_init__` refuses combinations it cannot vouch for.

    `refine_tolerance_s` bounds the error on each boundary. One second is well
    below anything the planner can act on, and bisection reaches it in about six
    extra propagations per boundary.
    """

    elevation_mask_deg: float = 5.0
    coarse_step_s: float = 60.0
    refine_tolerance_s: float = 1.0

    # Above this mask, passes get short enough that a 60-second coarse step is
    # no longer obviously safe, and the caller has to think rather than inherit
    # a default that happens to work at 5 degrees.
    _STEP_SAFE_BELOW_MASK_DEG = 30.0

    def __post_init__(self) -> None:
        if not -90.0 < self.elevation_mask_deg < 90.0:
            msg = f"elevation mask out of range: {self.elevation_mask_deg}"
            raise ValueError(msg)
        if self.coarse_step_s <= 0 or self.refine_tolerance_s <= 0:
            msg = "step and tolerance must be positive"
            raise ValueError(msg)
        if self.refine_tolerance_s >= self.coarse_step_s:
            msg = (
                "refine_tolerance_s must be smaller than coarse_step_s, "
                f"got {self.refine_tolerance_s} >= {self.coarse_step_s}"
            )
            raise ValueError(msg)
        if self.elevation_mask_deg > self._STEP_SAFE_BELOW_MASK_DEG and self.coarse_step_s > 30.0:
            msg = (
                f"an elevation mask above {self._STEP_SAFE_BELOW_MASK_DEG} degrees shortens "
                f"passes below what a {self.coarse_step_s}s coarse step can be trusted to "
                "bracket; lower the step deliberately rather than inheriting the default"
            )
            raise ValueError(msg)


@dataclass(frozen=True)
class AccessWindow:
    """One interval during which the target is above the elevation mask."""

    satellite_id: str
    start: datetime
    end: datetime
    peak_elevation_deg: float
    peak_at: datetime
    orbit_number: int

    # True when the window was cut by the horizon rather than by the geometry:
    # the satellite was already up when the search began, or still up when it
    # ended. The distinction matters downstream — a clipped window is not
    # evidence of a short pass, and treating it as one would understate what the
    # satellite can actually do.
    clipped_at_start: bool = False
    clipped_at_end: bool = False

    @property
    def duration_s(self) -> float:
        return (self.end - self.start).total_seconds()


def _iterate(start: datetime, end: datetime, step_s: float) -> list[datetime]:
    """Inclusive sample grid. The final sample lands exactly on `end`.

    Landing exactly on the end matters: without it a window that opens in the
    last partial step is invisible, and the bug only shows for horizons that are
    not a whole number of steps long — which is most of them.
    """
    out: list[datetime] = []
    step = timedelta(seconds=step_s)
    t = start
    while t < end:
        out.append(t)
        t += step
    out.append(end)
    return out


def search(
    propagator: Propagator,
    target: GroundPoint,
    horizon_start: datetime,
    horizon_end: datetime,
    policy: AccessSearchPolicy | None = None,
) -> list[AccessWindow]:
    """Find every access window for one satellite over one horizon.

    Deterministic: the sample grid is a pure function of the inputs, bisection
    is deterministic, and nothing consults the clock.
    """
    policy = policy or AccessSearchPolicy()

    # Awareness is checked BEFORE ordering. Comparing a naive datetime with an
    # aware one raises TypeError, so validating the order first would replace a
    # message naming the actual mistake with one about offsets.
    for label, when in (("horizon_start", horizon_start), ("horizon_end", horizon_end)):
        if when.tzinfo is None:
            msg = f"{label} must be timezone-aware"
            raise ValueError(msg)

    if horizon_end <= horizon_start:
        msg = "horizon_end must be after horizon_start"
        raise ValueError(msg)

    samples = _iterate(horizon_start, horizon_end, policy.coarse_step_s)
    elevations = propagator.elevations_deg(target, samples)
    mask = policy.elevation_mask_deg
    above = [e >= mask for e in elevations]

    windows: list[AccessWindow] = []
    open_at: datetime | None = None
    clipped_start = False

    for i in range(len(samples)):
        if above[i] and open_at is None:
            if i == 0:
                # Already up when the horizon began. There is no rise to find:
                # the boundary is the horizon itself, and saying otherwise would
                # invent a rise that happened before we were looking.
                open_at, clipped_start = samples[0], True
            else:
                open_at, clipped_start = (
                    _bisect(
                        propagator, target, samples[i - 1], samples[i], mask, policy, rising=True
                    ),
                    False,
                )
        elif not above[i] and open_at is not None:
            closed_at = _bisect(
                propagator, target, samples[i - 1], samples[i], mask, policy, rising=False
            )
            windows.append(
                _build(propagator, target, open_at, closed_at, policy, clipped_start, False)
            )
            open_at, clipped_start = None, False

    if open_at is not None:
        # Still up when the horizon ran out.
        windows.append(
            _build(propagator, target, open_at, samples[-1], policy, clipped_start, True)
        )

    return windows


def _bisect(
    propagator: Propagator,
    target: GroundPoint,
    before: datetime,
    after: datetime,
    mask: float,
    policy: AccessSearchPolicy,
    *,
    rising: bool,
) -> datetime:
    """Converge on the instant elevation crosses the mask, within tolerance.

    `before` is known to be on one side of the mask and `after` on the other, so
    the crossing is bracketed and bisection cannot diverge. The loop bound is
    the tolerance, not an iteration count, so tightening the tolerance costs
    propagations rather than silently returning a coarser answer.
    """
    low, high = before, after
    while (high - low).total_seconds() > policy.refine_tolerance_s:
        mid = low + (high - low) / 2
        elevation = propagator.elevations_deg(target, [mid])[0]
        is_above = elevation >= mask
        # Rising: keep the half that still contains the below-to-above step.
        if is_above == rising:
            high = mid
        else:
            low = mid
    # Return the side that is inside the window, so a reported window never
    # claims visibility the geometry does not support.
    return high if rising else low


def _build(
    propagator: Propagator,
    target: GroundPoint,
    start: datetime,
    end: datetime,
    policy: AccessSearchPolicy,
    clipped_start: bool,
    clipped_end: bool,
) -> AccessWindow:
    """Assemble a window, finding its peak elevation on a fine grid."""
    # Peak matters for quality scoring and, through the incidence relation, for
    # whether the SAR filter in M1-11 will keep anything at all. A tenth of the
    # coarse step is fine enough to place it without another bisection.
    step = max(policy.coarse_step_s / 10.0, policy.refine_tolerance_s)
    samples = _iterate(start, end, step)
    elevations = propagator.elevations_deg(target, samples)
    peak_index = max(range(len(elevations)), key=elevations.__getitem__)

    return AccessWindow(
        satellite_id=propagator.element_set.name,
        start=start,
        end=end,
        peak_elevation_deg=elevations[peak_index],
        peak_at=samples[peak_index],
        orbit_number=propagator.orbit_number(samples[peak_index]),
        clipped_at_start=clipped_start,
        clipped_at_end=clipped_end,
    )


@dataclass(frozen=True)
class HorizonPolicy:
    """How far ahead a sweep is willing to look.

    An unbounded horizon is a denial-of-service on ourselves: propagation cost
    is linear in horizon length and a customer asking for a year would occupy a
    consumer until its `ack_wait` expired, at which point the work is redelivered
    and done again, forever. Clamping turns that into a bounded answer with a
    flag on it.

    The default of 72 hours is not arbitrary — it is the point at which the TLE
    that produced the sweep would itself be STALE under the default
    `StalenessPolicy`. Looking further ahead than the orbital data can support
    would produce windows nobody should act on.
    """

    max_hours: float = 72.0

    def __post_init__(self) -> None:
        if self.max_hours <= 0:
            msg = f"max_hours must be positive, got {self.max_hours}"
            raise ValueError(msg)

    def clamp(self, start: datetime, end: datetime) -> tuple[datetime, bool]:
        """Return the effective end and whether the request was cut short."""
        limit = start + timedelta(hours=self.max_hours)
        if end > limit:
            return limit, True
        return end, False


@dataclass(frozen=True)
class SweepResult:
    """Every access window for a constellation over one clamped horizon."""

    windows: list[AccessWindow]
    horizon_start: datetime
    horizon_end: datetime
    truncated: bool
    satellites_evaluated: int

    @property
    def window_count(self) -> int:
        return len(self.windows)


def sweep(
    propagators: Sequence[Propagator],
    target: GroundPoint,
    horizon_start: datetime,
    horizon_end: datetime,
    policy: AccessSearchPolicy | None = None,
    horizon_policy: HorizonPolicy | None = None,
) -> SweepResult:
    """Search a whole constellation, clamping the horizon first.

    Windows come back ordered by start time across all satellites, because the
    planner batches by time bucket and an unordered list would just be sorted
    again by whoever consumed it.
    """
    horizon_policy = horizon_policy or HorizonPolicy()
    effective_end, truncated = horizon_policy.clamp(horizon_start, horizon_end)

    windows: list[AccessWindow] = []
    for propagator in propagators:
        windows.extend(search(propagator, target, horizon_start, effective_end, policy))

    # Sort by (start, satellite) rather than start alone: two satellites can
    # rise at the same instant, and a tie broken by list order would make the
    # result depend on the order propagators were passed in — which would break
    # determinism in a way no single-satellite test would catch.
    windows.sort(key=lambda w: (w.start, w.satellite_id))

    return SweepResult(
        windows=windows,
        horizon_start=horizon_start,
        horizon_end=effective_end,
        truncated=truncated,
        satellites_evaluated=len(propagators),
    )
