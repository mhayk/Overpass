-- One ACTIVE acquisition per request, GLOBALLY — the database half of #163.
--
-- SPEC §7.1 states the constraint out loud: at most one acquisition per
-- request, do not image the same target twice. The per-round Schedule enforces
-- it within one plan, and the first full-stack demo showed that is not enough:
-- rounds are partitioned by (satellite_id, bucket_start), each holds its own
-- advisory lock, and a request with candidates on four satellites was awarded
-- five ACTIVE acquisitions. Every test passed, because every test ran a single
-- satellite. The invariant that failed is the COMPOSITION across partitions,
-- and per ADR-0003 an invariant of that rank lives in the database, where no
-- code path, race or refactor can step around it.
--
-- The shape deliberately mirrors 00004's non-overlap constraint, because the
-- reasoning transfers verbatim:
--
--   PARTIAL over the live set. History does not contend: a SUPERSEDED or
--   EXECUTED row is a record, not a claim on the request. (EXECUTED is a real
--   question — an executed acquisition arguably satisfies the request forever —
--   but that is a product decision about re-tasking, not a uniqueness detail,
--   and the M4 execution simulator is where it gets decided.)
--
--   DEFERRABLE INITIALLY DEFERRED, because supersession must not acquire a
--   statement-ordering contract. A re-plan demotes the old plan's rows and
--   inserts the new plan's in one transaction; with an immediate constraint the
--   insert of a re-winning request would collide with its own not-yet-demoted
--   row, and the commit path would grow the exact hidden ordering dependency
--   ADR-0012 documented and rejected. Deferred, the check runs at COMMIT, after
--   both statements, in either order.
--
--   EXCLUDE USING gist rather than a partial unique index, because a unique
--   INDEX cannot be deferred — only constraints can, and UNIQUE constraints
--   cannot be partial. The gist exclusion with equality is the one form that is
--   both partial and deferrable. btree_gist (01-extensions.sql) supplies the
--   uuid equality operator class.
--
-- The planner-side read filter is the MECHANISM; this is the backstop. If it
-- ever fires, two rounds on different satellites awarded one request in the
-- same instant — the round that loses aborts whole, its bucket stays dirty,
-- and the next sweep retries with the filter now seeing the winner.

-- +goose Up

ALTER TABLE planning.acquisitions
    ADD CONSTRAINT acquisitions_one_active_per_request
    EXCLUDE USING gist (request_id WITH =)
    WHERE (status = 'ACTIVE')
    DEFERRABLE INITIALLY DEFERRED;

-- +goose Down

ALTER TABLE planning.acquisitions
    DROP CONSTRAINT acquisitions_one_active_per_request;
