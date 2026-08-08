-- plan-gateway read model: where the satellites actually were.
--
-- 00007 gave the gateway everything the planner COMMITTED. It gave it nothing
-- the acquisitions were derived FROM, so a CZML document could draw footprints
-- and a clock but no orbit — and interpolating a path through footprint
-- centroids would have drawn a curve that looks like an orbit and is not one.
-- This is the missing input, projected from feasibility.ephemeris.computed.v1.
--
-- ONE ROW PER SAMPLE, not one row per published track.
--
-- The event arrives as a bucket of samples, so a jsonb column holding the whole
-- track would be the shape of the message. It is the wrong shape for the
-- question: the renderer wants the samples covering a PLAN's window, and plan
-- buckets are not obliged to align with ephemeris buckets. A row per sample
-- makes that an ordinary range scan instead of fetching overlapping blobs and
-- clipping them in Go. It is also what makes the fold idempotent for free —
-- the primary key is the identity of the fact.
--
-- The cost is row count, and it is affordable: twelve satellites sampled every
-- ten seconds is about 104k rows per day, and the FEASIBILITY stream retains
-- 72 hours. See docs/decisions/0016-ephemeris-sampling-and-horizon.md.

-- +goose Up

CREATE TABLE readmodel.ephemeris (
    satellite_id  text        NOT NULL,

    -- The instant this sample describes, absolute. The event carries offsets
    -- from an epoch because a timestamp per sample is more bytes than the
    -- position it labels; that is a wire concern and it stops at the boundary.
    -- Storing offsets here would make every read reconstruct times before it
    -- could filter on them, which is exactly the index this table needs.
    sample_at     timestamptz NOT NULL,

    longitude_deg double precision NOT NULL CHECK (longitude_deg BETWEEN -180 AND 180),
    latitude_deg  double precision NOT NULL CHECK (latitude_deg  BETWEEN  -90 AND  90),

    -- Height above the WGS84 ellipsoid, metres. Not above terrain.
    altitude_m    double precision NOT NULL,

    -- Plain columns rather than a PostGIS geometry, deliberately. Nothing asks a
    -- spatial question of a satellite position — no containment, no intersection,
    -- no distance — and the renderer wants three numbers. A geometry(PointZ)
    -- would cost an ST_AsGeoJSON parse on every read for a capability nothing
    -- uses. Footprints are PostGIS because footprints are genuinely spatial.

    -- Which element set produced this sample, and the guard that lets a fresher
    -- one win.
    --
    -- Load-bearing. A satellite gets a new TLE roughly daily, and the next sweep
    -- republishes the same buckets from it — same satellite, same instants,
    -- slightly different positions. Without this the upsert below would have to
    -- either overwrite blindly (so a redelivered OLD track beats a new one,
    -- depending on arrival order) or DO NOTHING (so the stale track is kept
    -- forever).
    -- Comparing epochs makes the fold converge on the newest element set
    -- whatever order the events arrive in, which is the same property every
    -- other projection in this schema has.
    tle_epoch     timestamptz NOT NULL,

    -- The event's occurred_at, for staleness reporting. Not now().
    last_event_at timestamptz NOT NULL,

    PRIMARY KEY (satellite_id, sample_at)
);

-- The primary key already serves (satellite_id, sample_at) range scans, which
-- is the only read this table has: "every sample for satellite X between A and
-- B, in order". No second index — an index nobody queries is write cost and
-- disk for nothing, and this table is written 104k times a day.

COMMENT ON TABLE readmodel.ephemeris IS
    'Sampled satellite positions projected from feasibility.ephemeris.computed.v1. Nothing here is authoritative: feasibility owns the propagation, this is a projection of it, and it is rebuildable by replaying the stream.';

-- +goose Down

DROP TABLE readmodel.ephemeris;
