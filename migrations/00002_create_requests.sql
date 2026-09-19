-- +goose Up

-- One captured request. Append-only: rows are inserted and eventually expired,
-- never updated.
create table requests (
    id uuid primary key default uuidv7(),

    endpoint_id uuid not null references endpoints(id) on delete cascade,

    method text not null,

    -- The PERCENT-ENCODED path, not the decoded one. r.URL.Path is decoded, so
    -- a request for /a%00b would put a NUL byte in a Go string -- and Postgres
    -- rejects NUL in text outright. The escaped form is also closer to what was
    -- literally on the wire, which is what an inspector should show.
    path text not null,
    query text not null default '',

    -- An ARRAY of {name, value} objects, not an object keyed by name.
    --
    -- jsonb sorts object keys and collapses duplicates, which would destroy
    -- both the ordering and the repeated headers that HTTP explicitly allows.
    -- jsonb ARRAYS preserve element order and duplicates exactly, so wrapping
    -- the pairs in an array sidesteps the lossiness while keeping the column
    -- queryable and indexable.
    headers jsonb not null default '[]'::jsonb,

    -- bytea, not text and not jsonb. Signature verification in Phase 4 runs
    -- over the exact bytes received; a body is frequently not JSON and not
    -- always valid UTF-8. text would reject a NUL byte and jsonb would reorder
    -- keys, either of which breaks HMAC for reasons that look inexplicable.
    body bytea,
    body_size integer not null default 0,
    body_truncated boolean not null default false,

    -- The sender's Content-Length. Null under chunked encoding. This is the
    -- sender's CLAIM -- compare it against body_size rather than trusting it.
    declared_size bigint,

    -- inet, not text: Postgres validates it, normalises it, and can do subnet
    -- containment later for rate limiting. Null when RemoteAddr was unparseable.
    source_ip inet,

    received_at timestamptz not null default now(),

    -- What we answered the sender. Null until the response is decided.
    responded_status integer
);

-- The only index this table needs, and the column order is the entire design.
--
-- Every read is "the newest N for this inbox": endpoint_id equality, then
-- received_at ordering. With this order Postgres seeks to where the endpoint
-- begins and walks -- no sort step, no rows read and discarded. Reversed, one
-- inbox's rows scatter across the whole tree and the index stops helping.
create index requests_endpoint_received_idx
    on requests (endpoint_id, received_at desc);

-- +goose Down
drop table requests;
