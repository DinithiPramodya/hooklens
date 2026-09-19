-- +goose Up

-- How long an inbox keeps its captures.
--
-- Per-endpoint rather than one global constant, because the right answer
-- differs: a throwaway debugging inbox wants hours, one wired into a staging
-- integration wants weeks. A default with a NOT NULL means existing rows are
-- filled without a separate backfill step -- cheap here in a way it was not for
-- owner_token_hash in 00003, because there IS a correct value for every row.
alter table endpoints
    add column retention_hours integer not null default 168  -- 7 days
    check (retention_hours between 1 and 8760);              -- 1 hour .. 1 year

-- Deliberately NO index on requests.received_at alone.
--
-- The sweep filters on it across all endpoints, so an index would help that
-- query. But the sweep runs every few minutes and can afford a scan, while
-- every single capture would pay to maintain the index forever. One index on
-- an append-heavy table, as argued in 07.
--
-- The real answer at scale is time-based partitioning: DROP PARTITION is
-- instant and takes no locks, where DELETE writes a tombstone per row and
-- leaves the work to vacuum. Out of scope here, and noted so the ceiling of
-- this design is written down rather than discovered.

-- +goose Down
alter table endpoints drop column retention_hours;
