-- Per-satellite agility: what slew_time(a, b) is computed from.
--
-- M2-02 models the transition between two acquisitions on one satellite as a
-- function of the PAIR, not as a constant gap. A constant gap collapses the
-- problem to ordinary interval scheduling and removes the only genuinely hard
-- constraint the system has — ADR-0007's strong NP-hardness reduction runs
-- through exactly this term, so a constant here would quietly make the whole
-- allocation argument false.
--
-- These live on reference.satellites rather than in the sensor_modes JSONB
-- beside them, and the split is deliberate. ADR-0004 permits JSONB for per-mode
-- sensor parameters because those genuinely differ by mode and keep changing.
-- Agility does not: a spacecraft has one slew rate and one settling time,
-- they are scalars with real physical bounds, and the planner reads them on
-- every pairwise transition in a round. A negative slew rate in JSONB is an
-- invariant enforced in application code, which is the thing 00001's own
-- comments argue against; as a column it is enforced by the database and
-- unbypassable.
--
-- mode_transition_s is the exception that proves the split: it is charged when
-- the imaging MODE changes, so it is arguably per-mode-pair. It is a scalar
-- here because a full mode-pair matrix is unjustified for three modes and one
-- number is defensible until the benchmark shows otherwise. Stated so that
-- promoting it later is a decision rather than a discovery.

-- +goose Up

ALTER TABLE reference.satellites
    -- Degrees per second of roll. Strictly positive: a satellite that cannot
    -- slew cannot image off-nadir at all, and modelling that as 0 would divide
    -- by zero in the transition function rather than saying so.
    --
    -- The DEFAULT is what makes this migration applicable to existing rows, and
    -- it is a plausible small-SAR figure rather than a measured one. Any
    -- constellation that cares must set it per satellite; the benchmark in
    -- M2-13 is where a wrong value would show up as an implausible plan.
    ADD COLUMN slew_rate_deg_s   numeric NOT NULL DEFAULT 1.0
                                 CHECK (slew_rate_deg_s > 0),

    -- Settling time after the slew completes, before imaging can begin.
    --
    -- SEPARATE from the slew, not folded into an effective rate. A satellite
    -- that has finished rotating is not yet stable enough to image, and the
    -- distinction is load-bearing for the model: settling is a constant floor
    -- paid on EVERY transition including a zero-angle one, while slew time
    -- scales with the roll delta. Merging them would make slew_time(a, a) equal
    -- zero, and back-to-back acquisitions at identical geometry would appear
    -- free.
    --
    -- Zero is permitted. It is physically optimistic rather than impossible,
    -- and forbidding it would stop the benchmark isolating the slew term.
    ADD COLUMN settle_time_s     numeric NOT NULL DEFAULT 5.0
                                 CHECK (settle_time_s >= 0),

    -- Extra time charged when the imaging mode changes between two consecutive
    -- acquisitions — reconfiguring the radar, not moving the spacecraft.
    --
    -- Defaults to 0 so that a constellation which has not characterised it gets
    -- a model with one fewer unverified constant, rather than a plausible number
    -- nobody measured. A zero here is visible in the plan as mode changes being
    -- free, which is a question someone will ask.
    ADD COLUMN mode_transition_s numeric NOT NULL DEFAULT 0
                                 CHECK (mode_transition_s >= 0);

-- Bound the roll authority, so the derived roll angle can be checked against
-- something rather than trusted.
--
-- 60 degrees mirrors the contract: sar.v1.schema.json bounds roll_angle_deg to
-- [-60, 60]. Restated here because the planner DERIVES roll for candidates whose
-- optional roll_angle_deg was absent, and a derivation that silently exceeds the
-- spacecraft's authority would produce a plan that cannot be flown while looking
-- entirely reasonable.
ALTER TABLE reference.satellites
    ADD COLUMN max_roll_deg numeric NOT NULL DEFAULT 45.0
                            CHECK (max_roll_deg > 0 AND max_roll_deg <= 60);

-- +goose Down

ALTER TABLE reference.satellites
    DROP COLUMN max_roll_deg,
    DROP COLUMN mode_transition_s,
    DROP COLUMN settle_time_s,
    DROP COLUMN slew_rate_deg_s;
