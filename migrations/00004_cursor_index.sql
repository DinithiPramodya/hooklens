-- +goose Up

-- Add the cursor tiebreaker to the index.
--
-- Cursor pagination compares the tuple (received_at, id), because received_at
-- alone is not unique -- two requests can arrive in the same microsecond, and a
-- cursor that cannot distinguish them either skips a row or loops on it.
--
-- For the comparison to be served entirely by the index, id has to be IN the
-- index. With only (endpoint_id, received_at desc), Postgres can seek on the
-- timestamp but must then re-check and sort rows sharing one. That set is
-- usually tiny, so the old index was not wrong -- this makes it exact.
--
-- Directions must match the ORDER BY exactly (both desc). A B-tree can be
-- walked backwards, so a wholly reversed index would also serve -- but a
-- MIXED-direction index cannot serve a mixed-direction sort, and getting that
-- subtly wrong reintroduces a Sort node that nothing tests for.
create index requests_endpoint_cursor_idx
    on requests (endpoint_id, received_at desc, id desc);

drop index requests_endpoint_received_idx;

-- +goose Down
create index requests_endpoint_received_idx
    on requests (endpoint_id, received_at desc);

drop index requests_endpoint_cursor_idx;
